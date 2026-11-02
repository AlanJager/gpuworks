import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { smallFleet } from "./helpers/overviewFixtures.ts";

// The scheduling center: the overview's loading / empty / error / success
// states, the fleet summary, the heatmap with its filters, the node drawer that
// reads one node in full, the queues (pending reason worded like the deployment
// drawer words it, upcoming, waiting wakes cancelled from the keyboard) and the
// sharing stats.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation, useNavigate } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: SchedulingPage } = await import("../src/pages/SchedulingPage.tsx");
type Board = import("../src/schedulingApi.ts").SchedulingBoard;
type Overview = import("../src/schedulingApi.ts").SchedulingOverview;
type Timeline = import("../src/schedulingApi.ts").SchedulingTimeline;
type Activity = import("../src/schedulingApi.ts").SchedulingActivity;

const GIB = 1024 ** 3;
const s = locale.scheduling;
const original = { endWakeWaiting: api.endWakeWaiting };
const originalScheduling = { ...schedulingApi };

/** The fleet-wide lists of the board, as GET /scheduling/board?queues=1 answers. */
function queues(overrides: Partial<Board> = {}): Board {
  return {
    nodes: { items: [], page: 1, page_size: 10, total: 0 },
    pending: [{ deployment_id: 9, deployment: "big-model", reason: { code: "pool_empty", params: { nodes: 3, unmet: 1, cordoned: 1, offline: 1 } }, at: "2026-10-25T08:00:00Z" }],
    waiting_wakes: [{ deployment_id: 8, deployment: "llama", instances: ["inst-llama-0001"], since: "2026-10-25T09:00:00Z" }],
    upcoming: [
      { deployment_id: 7, deployment: "qwen", schedule_id: 1, action: "sleep", sleep_level: 2, at: "2026-10-25T22:00:00Z" },
      { deployment_id: 7, deployment: "qwen", schedule_id: 2, action: "replicas", replicas: 3, at: "2026-10-26T06:00:00Z" },
    ],
    ...overrides,
  };
}

/** One node read in full, as GET /scheduling/board?node= answers. */
function nodeBoard(uuid: string): Board {
  return queues({
    nodes: {
      items: [{
        uuid, name: "gpu-a", online: true,
        gpus: [
          {
            index: 0, name: "L20", memory_bytes: 48 * GIB, awake_bytes: 20 * GIB, sleeping_bytes: 4 * GIB, free_bytes: 24 * GIB, state: "awake", held_outside: false, switching: false,
            occupants: [
              { deployment_id: 7, deployment: "qwen", yield_policy: "idle", instance: "inst-qwen-0001", state: "awake", claim_bytes: 20 * GIB },
              { deployment_id: 8, deployment: "llama", yield_policy: "timeslice", instance: "inst-llama-0001", state: "asleep", sleep_level: 2, claim_bytes: 20 * GIB, residue_bytes: 4 * GIB },
            ],
          },
          { index: 1, name: "L20", memory_bytes: 48 * GIB, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 40 * GIB, state: "held_outside", held_outside: true, switching: false, occupants: [] },
        ],
      }],
      page: 1, page_size: 1, total: 1,
    },
    pending: [], waiting_wakes: [], upcoming: [],
  });
}

const SHARING_WINDOWS = [{ key: "15m", seconds: 900 }, { key: "1h", seconds: 3600 }, { key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }];
const sharing = (key = "6h"): Timeline => ({
  scope: "fleet",
  window: { from: "2026-10-28T06:00:00Z", to: "2026-10-28T12:00:00Z", hours: 6, key, seconds: SHARING_WINDOWS.find((w) => w.key === key)?.seconds ?? 21600, custom: false },
  windows: SHARING_WINDOWS, nodes: [], switches_available: true,
  deployments: [{ deployment_id: 8, deployment: "llama", yield_policy: "timeslice", awake_seconds: 3600, awake_share: 0.25, present_seconds: 14400, switches: 3, yielded: 2, yielded_to: 1, wakes: 1, held_requests: 6, wait_p50_ms: 4200, wait_p95_ms: 31000 }],
});

interface Calls { board: unknown[]; timeline: unknown[]; activity: unknown[] }

const activity = (): Activity => ({
  from: "2026-10-27T12:00:00Z", to: "2026-10-28T12:00:00Z", bucket_seconds: 900,
  reasons: [{ key: "idle_yield", count: 0 }, { key: "rank_preemption", count: 0 }], buckets: [], top_cards: [], beyond_retention: false,
});

function serve(options: { overview?: () => Promise<Overview>; board?: () => Board; sharing?: (key?: string) => Timeline } = {}): Calls {
  const calls: Calls = { board: [], timeline: [], activity: [] };
  // The activity card has its own tests (schedulingActivity.test.tsx).
  schedulingApi.activity = async (range) => {
    calls.activity.push(range);
    return activity();
  };
  schedulingApi.overview = options.overview ?? (async () => smallFleet());
  schedulingApi.board = async (query) => {
    calls.board.push(query);
    if ("node" in query) return nodeBoard(query.node);
    if ("queues" in query) return (options.board ?? queues)();
    throw new Error(`the page must not ask for a page of nodes: ${JSON.stringify(query)}`);
  };
  schedulingApi.timeline = async (query) => {
    calls.timeline.push(query);
    if ("sharing" in query) return (options.sharing ?? sharing)(query.window);
    return {
      scope: "node",
      window: { from: "2026-10-28T06:00:00Z", to: "2026-10-28T12:00:00Z", hours: 6, key: "6h", seconds: 21600, custom: false },
      windows: [{ key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }], nodes: [], deployments: [], switches_available: true,
    };
  };
  // The scheduling history has its own tests (schedulingHistory.test.tsx).
  schedulingApi.history = async () => ({ items: [{
    id: 1, created_at: "2026-10-25T07:00:00Z", deployment_id: 7, deployment: "qwen", kind: "automation", code: "AUTO_SLEEP", category: "auto",
    params: { trigger: "idle", level: 2, idle_sec: 900 }, nodes: [{ uuid: "node-c", name: "gpu-c" }],
  }] });
  return calls;
}

function Probe() {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <div data-testid="loc">{location.pathname}{location.search}</div>
      <button type="button" onClick={() => navigate(-1)}>browser back</button>
    </>
  );
}

function mount(path = "/scheduling") {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/scheduling" element={<SchedulingPage />} />
          <Route path="*" element={<div>elsewhere</div>} />
        </Routes>
        <Probe />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

afterEach(() => {
  cleanup();
  Object.assign(api, original);
  Object.assign(schedulingApi, originalScheduling);
});
after(() => closeChannels());

const cells = () => [...document.querySelectorAll<HTMLButtonElement>("button.hm-cell")];
const status = () => screen.getAllByRole("status").map((el) => el.textContent).join(" | ");

/** choose picks an option of an antd select by its label. */
async function choose(name: string, option: string) {
  fireEvent.mouseDown(screen.getByRole("combobox", { name }));
  const found = await waitFor(() => {
    const el = document.querySelector<HTMLElement>(`.ant-select-item-option[title="${option}"]`);
    assert.ok(el, `no option ${option}`);
    return el;
  });
  fireEvent.click(found);
}

test("a loading status shows until the overview answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  serve({ overview: async () => { await gate; return smallFleet(); } });
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(s.page.loading));
  release();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(screen.queryByText(s.page.loading), null);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  serve({ overview: async () => { calls += 1; if (calls === 1) throw new Error("overview unavailable"); return smallFleet(); } });
  const user = mount();
  assert.ok(await screen.findByText(s.page.loadFailed));
  assert.ok(screen.getByText("overview unavailable"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(screen.queryByText(s.page.loadFailed), null);
});

test("an empty fleet says so, and every queue says it is empty", async () => {
  serve({
    overview: async () => ({ summary: { nodes: 0, online_nodes: 0, cards: 0, awake: 0, asleep: 0, free: 0, held_outside: 0, unhealthy: 0, offline: 0, contention: { waiting_wakes: 0, held_requests: 0, yields_last_hour: 0 } }, states: ["offline", "unhealthy", "held_outside", "awake", "asleep", "free"], models: [], deployments: [], nodes: [] }),
    board: () => queues({ pending: [], waiting_wakes: [], upcoming: [] }),
  });
  mount();
  await screen.findByText(s.nodes.empty);
  for (const text of [s.pending.empty, s.waiting.empty, s.upcoming.empty]) assert.ok(await screen.findByText(text), text);
  assert.equal(screen.queryByRole("group", { name: s.heatmap.aria }), null);
});

test("the summary counts the cards by state and the contention, with the queues' counts", async () => {
  serve();
  mount();
  const summary = await screen.findByRole("group", { name: s.summary.title });
  const tile = (group: string, label: string) => {
    const el = within(within(summary).getByRole("group", { name: group })).getByText(label).closest(".scheduling-tile") as HTMLElement;
    return el.querySelector("strong")?.textContent;
  };
  assert.equal(tile(s.summary.cardsTitle, s.summary.total), "6");
  assert.ok(within(summary).getByText(i18n.t("scheduling:summary.nodesOnline", { online: 2, count: 3 })));
  for (const [state, count] of [["awake", "2"], ["asleep", "1"], ["free", "1"], ["held_outside", "1"], ["offline", "1"]]) {
    assert.equal(tile(s.summary.cardsTitle, s.states[state as keyof typeof s.states]), count, state);
  }
  assert.equal(within(summary).queryByText(s.states.unhealthy), null, "no faulty card, no faulty tile");
  assert.equal(within(summary).queryByRole("list"), null, "no line of cards per GPU model");
  assert.equal(tile(s.summary.contentionTitle, s.summary.waitingWakes), "1");
  assert.equal(tile(s.summary.contentionTitle, s.summary.heldRequests), "3");
  assert.equal(tile(s.summary.contentionTitle, s.summary.yields), "2");
  await waitFor(() => assert.equal(tile(s.summary.contentionTitle, s.summary.pending), "1"));
  assert.equal(tile(s.summary.contentionTitle, s.summary.upcoming), "2");
});

test("the heatmap shows every card and says how many are shown; the page never reads a page of nodes", async () => {
  const calls = serve();
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.equal(cells().length, 6);
  assert.ok(status().includes(i18n.t("scheduling:heatmap.shown", { nodes: 3, cards: 6 })));
  assert.deepEqual(calls.board, [{ queues: true }]);
  const legend = screen.getByRole("list", { name: s.nodes.legend.title });
  for (const label of [s.states.awake, s.states.asleep, s.states.free, s.states.held_outside, s.states.offline, s.heatmap.legendSwitching]) assert.ok(within(legend).getByText(label), label);
});

test("filters narrow the heatmap by model, node label and state, and a search rings the deployment's cards", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.models, "H100");
  await waitFor(() => assert.equal(cells().length, 3));
  assert.ok(status().includes(i18n.t("scheduling:heatmap.shown", { nodes: 1, cards: 3 })));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));
  await waitFor(() => assert.equal(cells().length, 6));

  await choose(s.filters.label, "zone=a");
  await waitFor(() => assert.equal(cells().length, 2));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));

  await choose(s.filters.state, s.filters.stateOptions.offline);
  await waitFor(() => assert.equal(cells().length, 1));
  await user.click(screen.getByRole("button", { name: s.filters.reset }));

  await user.type(screen.getByRole("textbox", { name: s.filters.search }), "night");
  await waitFor(() => assert.ok(status().includes(i18n.t("scheduling:heatmap.highlighted", { cards: 2 }))));
  assert.equal(cells().filter((cell) => cell.classList.contains("is-hit")).length, 2);
  await user.clear(screen.getByRole("textbox", { name: s.filters.search }));
  await user.type(screen.getByRole("textbox", { name: s.filters.search }), "nothing like it");
  await waitFor(() => assert.ok(status().includes(s.heatmap.noDeployment)));
});

test("a filter that matches nothing says so instead of drawing an empty grid", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.models, "H100");
  await choose(s.filters.label, "zone=a");
  await screen.findByText(s.heatmap.emptyFiltered);
  assert.equal(screen.queryByRole("group", { name: s.heatmap.aria }), null);
  await user.click(screen.getByRole("button", { name: s.filters.reset }));
  await screen.findByRole("group", { name: s.heatmap.aria });
});

test("grouping by a label and sorting by contention reorder the nodes", async () => {
  serve();
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await choose(s.filters.groupBy, i18n.t("scheduling:filters.group.label", { key: "zone" }));
  const sections = await waitFor(() => {
    const found = screen.getAllByRole("region");
    assert.equal(found.length, 3);
    return found;
  });
  assert.deepEqual(sections.map((el) => el.getAttribute("aria-label")), ["a", "c", s.heatmap.noLabel]);
  await choose(s.filters.sortBy, s.filters.sort.contention);
  const order = () => [...document.querySelectorAll(".hm-node")].map((el) => el.getAttribute("aria-label"));
  await waitFor(() => assert.deepEqual(order(), ["gpu-a", "gpu-c", "gpu-b"]));
});

test("opening a node reads it in full: each GPU as a bar with its holders, and its residency timeline", async () => {
  const calls = serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  assert.ok(!calls.board.some((query) => "node" in (query as object)), "no node is read until one is opened");
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  const drawer = await screen.findByRole("dialog");
  await within(drawer).findByRole("region", { name: "gpu-a" });
  assert.deepEqual(calls.board.at(-1), { node: "node-a" });
  const bar = within(drawer).getByRole("img", { name: i18n.t("scheduling:nodes.barAria", { node: "gpu-a", index: 0, awake: "20.0 GiB", sleeping: "4.0 GiB", free: "24.0 GiB", total: "48.0 GiB" }) });
  assert.deepEqual([...bar.querySelectorAll("span")].map((span) => Math.round(parseFloat((span as HTMLElement).style.width))), [42, 8, 50]);
  const level = (name: string) => i18n.t("scheduling:nodes.occupant.level", { level: i18n.t(`deployments:yieldPolicy.policy.name.${name}`) }).trim();
  const awake = within(drawer).getByRole("link", { name: "qwen" }).parentElement as HTMLElement;
  assert.ok(within(awake).getByText(new RegExp(i18n.t("scheduling:nodes.occupant.awake", { claim: "20.0 GiB" }))));
  assert.ok(within(awake).getByText(level("idle")));
  const asleep = within(drawer).getByRole("link", { name: "llama" }).parentElement as HTMLElement;
  assert.ok(within(asleep).getByText(new RegExp(i18n.t("scheduling:nodes.occupant.asleep", { level: 2, residue: "4.0 GiB" }))));
  assert.ok(within(asleep).getByText(level("timeslice")));
  assert.ok(within(drawer).getByText(s.nodes.heldOutside));
  // Each card names the state the server gave it, the one its heatmap cell shows.
  assert.ok(within(drawer).getByText(s.states.awake, { selector: ".scheduling-gpu-state" }));
  assert.ok(within(drawer).getByText(s.states.held_outside, { selector: ".scheduling-gpu-state" }));
  assert.ok(within(drawer).getByText(s.timeline.title));
  await waitFor(() => assert.deepEqual(calls.timeline.find((query) => "nodes" in (query as object)), { nodes: ["node-a"], window: undefined }));
  await user.click(within(drawer).getByRole("link", { name: "qwen" }));
  assert.equal(screen.getByTestId("loc").textContent, "/deployments?deployment=7");
});

test("a node that cannot be read says so in its drawer and retries", async () => {
  serve();
  let calls = 0;
  const base = schedulingApi.board;
  schedulingApi.board = async (query) => {
    if ("node" in query) { calls += 1; if (calls === 1) throw new Error("node unavailable"); }
    return base(query);
  };
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  const drawer = await screen.findByRole("dialog");
  assert.ok(await within(drawer).findByText(s.drawer.loadFailed));
  assert.ok(within(drawer).getByText("node unavailable"));
  await user.click(within(drawer).getByRole("button", { name: s.page.retry }));
  await within(drawer).findByRole("region", { name: "gpu-a" });
});

test("the open node is in the URL: opening sets ?node=, closing clears it, and the browser goes back to it", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  await user.click(screen.getByRole("button", { name: new RegExp("^gpu-a GPU 0,") }));
  await within(await screen.findByRole("dialog")).findByRole("region", { name: "gpu-a" });
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-a");
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
  await waitFor(() => assert.equal(screen.getByTestId("loc").textContent, "/scheduling"));
  await waitFor(() => assert.equal(document.querySelector(".ant-drawer-open"), null));
  await user.click(screen.getByRole("button", { name: "browser back" }));
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-a");
  await within(await screen.findByRole("dialog")).findByRole("region", { name: "gpu-a" });
  await user.click(screen.getByRole("button", { name: "browser back" }));
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling");
  await waitFor(() => assert.equal(document.querySelector(".ant-drawer-open"), null));
});

test("a link to /scheduling?node= opens that node's drawer, titled with its name", async () => {
  const calls = serve();
  mount("/scheduling?node=node-c");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-c"));
  await waitFor(() => assert.deepEqual(calls.board.find((query) => "node" in (query as object)), { node: "node-c" }));
});

test("a node named in the scheduling history opens its drawer on the page", async () => {
  serve();
  const user = mount();
  const history = (await screen.findByText(s.history.title)).closest(".ant-card") as HTMLElement;
  await user.click(await within(history).findByRole("link", { name: "gpu-c" }));
  assert.equal(screen.getByTestId("loc").textContent, "/scheduling?node=node-c");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-c"));
});

test("a node can be opened from the keyboard alone", async () => {
  serve();
  const user = mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  cells()[0].focus();
  await user.keyboard("{ArrowRight}{ArrowRight}{Enter}");
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByText("gpu-b"), "the third card is gpu-b's, and its name titles the drawer");
});

test("pending reasons read like the deployment drawer's placement alert; upcoming entries are worded; the history sits below", async () => {
  serve();
  mount();
  await screen.findByText("big-model");
  assert.ok(screen.getByText(i18n.t("deploy:schedule.pool_empty", { nodes: 3, unmet: 1, cordoned: 1, offline: 1 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.sleep", { level: 2 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.replicas", { count: 3 })));
  assert.ok(await screen.findByText(i18n.t("deployments:instance.event.AUTO_SLEEP", {
    level: 2, cause: i18n.t("deployments:automation.cause.idle", { idle: "15 min" }),
  })));
  assert.ok(screen.getByText(s.history.title));
});

test("a PD role's scheduled count names the role, and a count of 0 is still a count", async () => {
  serve({
    board: () => queues({
      upcoming: [
        { deployment_id: 11, deployment: "pd-chat", schedule_id: 3, action: "replicas", replicas: 0, role: "prefill", at: "2026-10-26T07:00:00Z" },
        { deployment_id: 11, deployment: "pd-chat", schedule_id: 4, action: "replicas", replicas: 3, role: "decode", at: "2026-10-26T08:00:00Z" },
      ],
    }),
  });
  mount();
  for (const [role, count] of [["prefill", 0], ["decode", 3]] as const) {
    assert.ok(await screen.findByText(i18n.t("scheduling:upcoming.action.roleReplicas", { role: i18n.t(`deployments:detail.pd.role.${role}`), count })), role);
  }
});

test("a waiting wake is cancelled from the keyboard, then the queues and the overview reload", async () => {
  let waiting = queues().waiting_wakes;
  serve({ board: () => queues({ waiting_wakes: waiting }) });
  const cancelled: number[] = [];
  api.endWakeWaiting = async (id) => { cancelled.push(id); waiting = []; return { items: ["inst-llama-0001"], total: 1 }; };
  let reads = 0;
  const base = schedulingApi.overview;
  schedulingApi.overview = async () => { reads += 1; return base(); };
  const user = mount();
  await screen.findByText("big-model");
  const before = reads;
  const cancel = screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) });
  cancel.focus();
  assert.equal(document.activeElement, cancel);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(cancelled, [8]));
  await screen.findByText(s.waiting.empty);
  assert.ok(reads > before, "the overview is read again");
});

test("a refused cancel keeps the waiting wake and says why", async () => {
  serve();
  api.endWakeWaiting = async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409); };
  const user = mount();
  await screen.findByText("big-model");
  await user.click(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
  await screen.findByText(i18n.t("scheduling:waiting.cancelFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
  assert.ok(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
});

test("the sharing stats cover the fleet, are read for the chosen window, and say when they fail", async () => {
  let fail = false;
  const calls = serve({ sharing: (key) => { if (fail) throw new Error("log unavailable"); return sharing(key); } });
  const user = mount();
  const title = await screen.findByText(s.sharing.title);
  const card = title.closest(".ant-card") as HTMLElement;
  await within(card).findByRole("link", { name: "llama" });
  // The server says these are the fleet's figures: no node-only hint.
  assert.equal(within(card).queryByRole("img", { name: s.timeline.scopeNode }), null);
  // The first read leaves the window to the server; its answer lists the four
  // windows, 6 hours chosen.
  assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: undefined });
  const picker = within(card).getByRole("radiogroup", { name: s.timeline.window });
  assert.deepEqual(within(picker).getAllByRole("radio").map((radio) => (radio as HTMLInputElement).checked), [false, false, true, false]);
  assert.ok(within(picker).getByText(i18n.t("scheduling:timeline.minutes", { minutes: 15 })));
  await user.click(within(picker).getByText(i18n.t("scheduling:timeline.hours", { hours: 1 })));
  await waitFor(() => assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: "1h" }));
  fail = true;
  await user.click(within(card).getByText(i18n.t("scheduling:timeline.hours", { hours: 24 })));
  await waitFor(() => assert.deepEqual(calls.timeline.at(-1), { sharing: true, window: "24h" }));
  assert.ok(await within(card).findByText(i18n.t("scheduling:sharing.refreshFailed", { detail: "log unavailable" })), "the last stats stay under the warning");
  assert.ok(within(card).getByRole("link", { name: "llama" }));
});

test("a later failed refresh keeps the last overview and warns", async () => {
  let calls = 0;
  serve({ overview: async () => { calls += 1; if (calls > 1) throw new Error("server restarting"); return smallFleet(); } });
  mount();
  await screen.findByRole("group", { name: s.heatmap.aria });
  // The poll skips hidden documents, and jsdom's is hidden.
  Object.defineProperty(document, "hidden", { value: false, configurable: true });
  document.dispatchEvent(new dom.window.Event("visibilitychange"));
  await screen.findByText(i18n.t("scheduling:page.refreshFailed", { detail: "server restarting" }));
  assert.equal(cells().length, 6);
});
