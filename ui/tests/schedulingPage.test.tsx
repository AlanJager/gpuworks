import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The scheduling center: the board's loading / empty / error / success states,
// the GPU bars and their occupants, the link into a deployment, the pending
// reason worded like the deployment drawer words it, and the keyboard path to
// cancel a waiting wake.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: SchedulingPage } = await import("../src/pages/SchedulingPage.tsx");
type Board = import("../src/api.ts").SchedulingBoard;

const GIB = 1024 ** 3;
const s = locale.scheduling;
const original = { schedulingBoard: api.schedulingBoard, endWakeWaiting: api.endWakeWaiting, schedulingHistory: api.schedulingHistory };

function board(overrides: Partial<Board> = {}): Board {
  return {
    nodes: {
      items: [{
        uuid: "node-a", name: "gpu-a", online: true,
        gpus: [
          {
            index: 0, name: "RTX 8000", memory_bytes: 48 * GIB, awake_bytes: 20 * GIB, sleeping_bytes: 4 * GIB, free_bytes: 24 * GIB, held_outside: false,
            occupants: [
              { deployment_id: 7, deployment: "qwen", instance: "inst-qwen-0001", state: "awake", claim_bytes: 20 * GIB },
              { deployment_id: 8, deployment: "llama", instance: "inst-llama-0001", state: "asleep", sleep_level: 2, claim_bytes: 20 * GIB, residue_bytes: 4 * GIB },
            ],
          },
          { index: 1, name: "RTX 8000", memory_bytes: 48 * GIB, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 40 * GIB, held_outside: true, occupants: [] },
        ],
      }],
      page: 1, page_size: 10, total: 1,
    },
    pending: [{ deployment_id: 9, deployment: "big-model", reason: { code: "pool_empty", params: { nodes: 3, unmet: 1, cordoned: 1, offline: 1 } }, at: "2026-10-25T08:00:00Z" }],
    waiting_wakes: [{ deployment_id: 8, deployment: "llama", instances: ["inst-llama-0001"], since: "2026-10-25T09:00:00Z" }],
    upcoming: [
      { deployment_id: 7, deployment: "qwen", schedule_id: 1, action: "sleep", sleep_level: 2, at: "2026-10-25T22:00:00Z" },
      { deployment_id: 7, deployment: "qwen", schedule_id: 2, action: "replicas", replicas: 3, at: "2026-10-26T06:00:00Z" },
    ],
    recent: [{ deployment_id: 7, deployment: "qwen", code: "AUTO_SLEEP", params: { trigger: "idle", level: 2, idle_sec: 900 }, at: "2026-10-25T07:00:00Z" }],
    ...overrides,
  };
}

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount() {
  // The scheduling history has its own tests (schedulingHistory.test.tsx).
  api.schedulingHistory = async () => ({ items: [{
    id: 1, created_at: "2026-10-25T07:00:00Z", deployment_id: 7, deployment: "qwen", kind: "automation", code: "AUTO_SLEEP", category: "auto",
    params: { trigger: "idle", level: 2, idle_sec: 900 },
  }] });
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/scheduling"]}>
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
});
after(() => closeChannels());

test("a loading status shows until the board answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  api.schedulingBoard = async () => { await gate; return board(); };
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(s.page.loading));
  release();
  await screen.findByText("gpu-a");
  assert.equal(screen.queryByText(s.page.loading), null);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  api.schedulingBoard = async () => {
    calls += 1;
    if (calls === 1) throw new Error("board unavailable");
    return board();
  };
  const user = mount();
  assert.ok(await screen.findByText(s.page.loadFailed));
  assert.ok(screen.getByText("board unavailable"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await screen.findByText("gpu-a");
  assert.equal(screen.queryByText(s.page.loadFailed), null);
});

test("an empty board says so in every table and the node list", async () => {
  api.schedulingBoard = async () => board({
    nodes: { items: [], page: 1, page_size: 10, total: 0 },
    pending: [], waiting_wakes: [], upcoming: [], recent: [],
  });
  mount();
  await screen.findByText(s.nodes.empty);
  for (const text of [s.pending.empty, s.waiting.empty, s.upcoming.empty]) assert.ok(screen.getByText(text), text);
});

test("the summary counts what waits, and each GPU is a bar of awake, sleeping residue and free memory", async () => {
  api.schedulingBoard = async () => board();
  mount();
  await screen.findByText("gpu-a");
  const summary = screen.getByRole("group", { name: s.summary.title });
  for (const [label, count] of [[s.summary.waitingWakes, "1"], [s.summary.pending, "1"], [s.summary.upcoming, "2"]]) {
    const tile = within(summary).getByText(label).parentElement as HTMLElement;
    assert.equal(within(tile).getByText(count).textContent, count, label);
  }
  const bar = screen.getByRole("img", { name: i18n.t("scheduling:nodes.barAria", { node: "gpu-a", index: 0, awake: "20.0 GiB", sleeping: "4.0 GiB", free: "24.0 GiB", total: "48.0 GiB" }) });
  assert.equal(bar.querySelectorAll("span").length, 3);
  assert.deepEqual([...bar.querySelectorAll("span")].map((span) => Math.round(parseFloat((span as HTMLElement).style.width))), [42, 8, 50]);
  assert.ok(screen.getByText(i18n.t("scheduling:nodes.figures", { awake: "20.0 GiB", sleeping: "4.0 GiB", free: "24.0 GiB", total: "48.0 GiB" })));
  const legend = screen.getByRole("list", { name: s.nodes.legend.title });
  for (const label of [s.nodes.legend.awake, s.nodes.legend.sleeping, s.nodes.legend.free]) assert.ok(within(legend).getByText(label));
});

test("occupants show their state: the claim while awake, the residue while asleep; a card held outside warns", async () => {
  api.schedulingBoard = async () => board();
  mount();
  await screen.findByText("gpu-a");
  const node = within(screen.getByRole("region", { name: "gpu-a" }));
  const awake = node.getByRole("link", { name: "qwen" }).parentElement as HTMLElement;
  assert.ok(within(awake).getByText(new RegExp(i18n.t("scheduling:nodes.occupant.awake", { claim: "20.0 GiB" }))));
  const asleep = node.getByRole("link", { name: "llama" }).parentElement as HTMLElement;
  assert.ok(within(asleep).getByText(new RegExp(i18n.t("scheduling:nodes.occupant.asleep", { level: 2, residue: "4.0 GiB" }))));
  assert.ok(screen.getByText(s.nodes.heldOutside));
  assert.ok(screen.getByText(s.nodes.heldOutsideHint));
});

test("a deployment name opens that deployment's drawer", async () => {
  api.schedulingBoard = async () => board();
  const user = mount();
  await screen.findByText("gpu-a");
  await user.click(within(screen.getByRole("region", { name: "gpu-a" })).getByRole("link", { name: "qwen" }));
  assert.equal(screen.getByTestId("loc").textContent, "/deployments?deployment=7");
});

test("pending reasons read like the deployment drawer's placement alert; upcoming entries are worded; the history replaces the board's recent list", async () => {
  api.schedulingBoard = async () => board();
  mount();
  await screen.findByText("big-model");
  assert.ok(screen.getByText(i18n.t("deploy:schedule.pool_empty", { nodes: 3, unmet: 1, cordoned: 1, offline: 1 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.sleep", { level: 2 })));
  assert.ok(screen.getByText(i18n.t("scheduling:upcoming.action.replicas", { count: 3 })));
  // What was done lately is the scheduling history, worded as the timeline words it.
  assert.ok(await screen.findByText(i18n.t("deployments:instance.event.AUTO_SLEEP", {
    level: 2, cause: i18n.t("deployments:automation.cause.idle", { idle: "15 min" }),
  })));
  assert.ok(screen.getByText(s.history.title));
  assert.equal(screen.queryByText("Recent automation actions"), null);
});

test("the search narrows the nodes on the server and starts from the first page", async () => {
  const queries: unknown[] = [];
  api.schedulingBoard = async (query) => { queries.push(query); return board(); };
  const user = mount();
  await screen.findByText("gpu-a");
  await user.type(screen.getByRole("searchbox", { name: s.nodes.search }), "gpu-b{Enter}");
  await waitFor(() => assert.deepEqual(queries.at(-1), { page: 1, page_size: 10, q: "gpu-b" }));
});

test("a waiting wake is cancelled from the keyboard, then the board reloads", async () => {
  let waiting = board().waiting_wakes;
  api.schedulingBoard = async () => board({ waiting_wakes: waiting });
  const cancelled: number[] = [];
  api.endWakeWaiting = async (id) => { cancelled.push(id); waiting = []; return { items: ["inst-llama-0001"], total: 1 }; };
  const user = mount();
  await screen.findByText("big-model");
  const cancel = screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) });
  cancel.focus();
  assert.equal(document.activeElement, cancel);
  await user.keyboard("{Enter}");
  await waitFor(() => assert.deepEqual(cancelled, [8]));
  await screen.findByText(s.waiting.empty);
});

test("a refused cancel keeps the waiting wake and says why", async () => {
  api.schedulingBoard = async () => board();
  api.endWakeWaiting = async () => { throw new APIError("DEPLOYMENT_DELETING", {}, "the deployment is being deleted", 409); };
  const user = mount();
  await screen.findByText("big-model");
  await user.click(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
  await screen.findByText(i18n.t("scheduling:waiting.cancelFailed", { detail: i18n.t("common:apiError.DEPLOYMENT_DELETING") }));
  assert.ok(screen.getByRole("button", { name: i18n.t("scheduling:waiting.cancelOf", { name: "llama" }) }));
});

test("a later failed refresh keeps the last board and warns", async () => {
  let calls = 0;
  api.schedulingBoard = async () => {
    calls += 1;
    if (calls > 1) throw new Error("server restarting");
    return board();
  };
  mount();
  await screen.findByText("gpu-a");
  // The poll skips hidden documents, and jsdom's is hidden.
  Object.defineProperty(document, "hidden", { value: false, configurable: true });
  document.dispatchEvent(new dom.window.Event("visibilitychange"));
  await screen.findByText(i18n.t("scheduling:page.refreshFailed", { detail: "server restarting" }));
  assert.ok(screen.getByText("gpu-a"));
});
