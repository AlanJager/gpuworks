import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The scheduling history on the scheduling center: its loading / empty / error
// / success states, each entry worded like the deployment timeline words it,
// the filters (category, deployment, time range) from the keyboard, and the
// cursor paging.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: HistoryCard, historyWindow } = await import("../src/pages/scheduling/HistoryCard.tsx");
type Entry = import("../src/schedulingApi.ts").SchedulingHistoryEntry;
type Query = import("../src/schedulingApi.ts").SchedulingHistoryQuery;

const h = locale.scheduling.history;
const original = { deploymentsPage: api.deploymentsPage };
const originalScheduling = { ...schedulingApi };

function entry(id: number, overrides: Partial<Entry> = {}): Entry {
  return {
    id, created_at: "2026-10-25T07:00:00Z", deployment_id: 7, deployment: "qwen", kind: "automation", code: "AUTO_SLEEP", category: "auto",
    params: { trigger: "idle", level: 2, idle_sec: 900 }, ...overrides,
  };
}

const sample: Entry[] = [
  entry(30),
  entry(29, { code: "REPLICAS_CHANGED", kind: "schedule", category: "manual", params: { from: 1, to: 3, actor: "alice" }, actor: "alice", deployment_id: 8, deployment: "llama" }),
  entry(28, { code: "REPLICA_REBUILT", kind: "schedule", category: "failure", params: { replica: 1, attempt: 2, node: "gpu-3", reason: "CUDA out of memory" }, deployment_id: 9, deployment: "gone", deployment_deleted: true }),
  entry(27, { code: "NODE_REPLACED", category: "failure", params: { from: "gpu-1", instances: ["abcdef012345"], after_sec: 600 }, deployment_id: 10, deployment: undefined }),
];

function Probe() {
  const location = useLocation();
  return <div data-testid="loc">{location.pathname}{location.search}</div>;
}

function mount() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/scheduling"]}>
        <Routes>
          <Route path="/scheduling" element={<HistoryCard />} />
          <Route path="*" element={<div>elsewhere</div>} />
        </Routes>
        <Probe />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

// The segmented controls hide their native radios under a styled label.
const choose = (user: ReturnType<typeof userEvent.setup>, name: string) => user.click(screen.getByRole("radio", { name }).closest("label")!);

function serve(items: Entry[] | ((query: Query) => Entry[])) {
  const queries: Query[] = [];
  schedulingApi.history = async (query) => { queries.push(query); return { items: typeof items === "function" ? items(query) : items }; };
  api.deploymentsPage = async (query) => ({
    items: [{ id: 7, name: "qwen" }, { id: 8, name: "llama" }].filter((d) => !query.q || d.name.includes(query.q)),
    page: 1, page_size: 20, total: 2,
  } as never);
  return queries;
}

afterEach(() => {
  cleanup();
  Object.assign(api, original);
  Object.assign(schedulingApi, originalScheduling);
});
after(() => closeChannels());

test("a loading status shows until the history answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  schedulingApi.history = async () => { await gate; return { items: sample }; };
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(h.loading));
  release();
  await screen.findByText("qwen");
  assert.equal(screen.queryByText(h.loading), null);
});

test("each entry shows its time, deployment, action, worded details, category and actor", async () => {
  serve(sample);
  const user = mount();
  await screen.findByText("llama");
  const rows = screen.getAllByRole("row").slice(1);
  assert.equal(rows.length, 4);

  const autoSleep = within(rows[0]);
  assert.ok(autoSleep.getByText(i18n.t("scheduling:history.action.AUTO_SLEEP")));
  assert.ok(autoSleep.getByText(i18n.t("deployments:instance.event.AUTO_SLEEP", { level: 2, cause: i18n.t("deployments:automation.cause.idle", { idle: "15 min" }) })));
  assert.ok(autoSleep.getByText(h.category.auto));
  assert.equal(autoSleep.getAllByText("—").length, 1, "an automatic entry names no actor");

  // The actor is a column of its own, not repeated in the sentence.
  const scaled = within(rows[1]);
  assert.ok(scaled.getByText(i18n.t("deployments:instance.event.REPLICAS_CHANGED", { from: 1, to: 3, roleNote: "" })));
  assert.ok(scaled.getByText("alice"));
  assert.ok(scaled.getByText(h.category.manual));

  const rebuilt = within(rows[2]);
  assert.ok(rebuilt.getByText(i18n.t("scheduling:history.deleted", { name: "gone" })));
  assert.equal(rebuilt.queryByRole("link"), null, "a deleted deployment has no page to open");
  assert.ok(rebuilt.getByText(/CUDA out of memory/));
  assert.ok(rebuilt.getByText(h.category.failure));

  // A deployment whose name is unknown still shows which one.
  assert.ok(within(rows[3]).getByText("#10"));

  await user.click(within(rows[0]).getByRole("link", { name: "qwen" }));
  assert.equal(screen.getByTestId("loc").textContent, "/deployments?deployment=7");
});

test("an entry that gave GPUs to another deployment names it, and the name opens it", async () => {
  const preempt = (id: number, target: Entry["target"]) => entry(id, {
    params: { trigger: "preempt", reason: "rank", for_deployment: target?.id ?? 0, level: 1, instances: ["abcdef012345"] }, target,
  });
  serve([
    preempt(40, { id: 8, name: "llama" }),
    preempt(39, { id: 9, name: "gone", deleted: true }),
    preempt(38, { id: 11 }),
  ]);
  const user = mount();
  await screen.findByRole("link", { name: "llama" });
  const rows = screen.getAllByRole("row").slice(1);
  // The sentence reads as the timeline's, with the name where "#id" was.
  const sentence = i18n.t("deployments:instance.event.AUTO_SLEEP", {
    level: 1, instances: "abcdef01", cause: i18n.t("deployments:automation.preempt.rank", { who: "llama" }),
  });
  assert.equal(within(rows[0]).getByText((_, el) => el?.tagName === "SPAN" && el.textContent === sentence && el.querySelector("a") !== null) !== null, true);
  // A deleted one is named but has no page; an unknown one shows its id.
  assert.ok(within(rows[1]).getByText(i18n.t("scheduling:history.deleted", { name: "gone" })));
  assert.equal(within(rows[1]).queryAllByRole("link").map((a) => a.textContent).join(), "qwen");
  assert.ok(within(rows[2]).getByText(/#11/));
  await user.click(within(rows[0]).getByRole("link", { name: "llama" }));
  assert.equal(screen.getByTestId("loc").textContent, "/deployments?deployment=8");
});

test("an empty history says so, and says differently when filters hide everything", async () => {
  const queries = serve([]);
  const user = mount();
  await screen.findByText(h.empty);
  await choose(user, h.category.manual);
  await screen.findByText(h.emptyFiltered);
  assert.deepEqual(queries.at(-1)?.category, ["manual"]);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  schedulingApi.history = async () => {
    calls += 1;
    if (calls === 1) throw new Error("history unavailable");
    return { items: sample };
  };
  const user = mount();
  assert.ok(await screen.findByText(h.loadFailed));
  assert.ok(screen.getByText("history unavailable"));
  await user.click(screen.getByRole("button", { name: locale.scheduling.page.retry }));
  await screen.findByText("qwen");
  assert.equal(screen.queryByText(h.loadFailed), null);
});

test("a later failed refresh keeps the entries and warns", async () => {
  let calls = 0;
  schedulingApi.history = async () => {
    calls += 1;
    if (calls > 1) throw new Error("server restarting");
    return { items: sample };
  };
  mount();
  await screen.findByText("qwen");
  // The poll skips hidden documents, and jsdom's is hidden.
  Object.defineProperty(document, "hidden", { value: false, configurable: true });
  document.dispatchEvent(new dom.window.Event("visibilitychange"));
  await screen.findByText(i18n.t("scheduling:history.refreshFailed", { detail: "server restarting" }));
  assert.ok(screen.getByText("qwen"));
});

test("the category filter works from the keyboard and starts over from the newest entries", async () => {
  const queries = serve(sample);
  const user = mount();
  await screen.findByText("qwen");
  assert.equal(queries[0].category, undefined);
  // Tab onto the group, then Space on the focused option chooses it.
  const auto = screen.getByRole("radio", { name: h.category.auto }) as HTMLInputElement;
  auto.focus();
  assert.equal(document.activeElement, auto);
  await user.keyboard(" ");
  await waitFor(() => assert.deepEqual(queries.at(-1)?.category, ["auto"]));
  assert.equal(auto.checked, true);
  await choose(user, h.category.failure);
  await waitFor(() => assert.deepEqual(queries.at(-1)?.category, ["failure"]));
  assert.equal(queries.at(-1)?.before, undefined);
});

test("the time range is the platform's picker and asks for entries since that long ago", async () => {
  const queries = serve(sample);
  mount();
  await screen.findByText("qwen");
  assert.equal(queries[0].since, undefined);
  const box = screen.getByRole("combobox", { name: locale.deployments.timeRange.ariaLabel });
  fireEvent.mouseDown(box);
  const day = await waitFor(() => {
    const option = document.querySelector(`.ant-select-item-option[title="${locale.deployments.timeRange["1d"]}"]`);
    assert.ok(option, "the quick ranges are offered");
    return option;
  });
  assert.ok(document.querySelector(`.ant-select-item-option[title="${h.range.all}"]`), "all time is the first choice");
  assert.ok(document.querySelector(`.ant-select-item-option[title="${locale.deployments.timeRange.custom}"]`), "a custom range is offered");
  const before = Date.now();
  fireEvent.click(day!);
  await waitFor(() => assert.ok(queries.at(-1)?.since));
  const since = Date.parse(queries.at(-1)?.since ?? "");
  assert.ok(since >= before - 86_400_000 - 1000 && since <= Date.now() - 86_400_000 + 1000, `since = ${queries.at(-1)?.since}`);
  assert.equal(queries.at(-1)?.until, undefined, "a quick range slides with now");
});

test("a history window is open for all time, slides for a quick range and is fixed for a custom one", () => {
  const now = Date.parse("2026-10-30T12:00:00Z");
  assert.deepEqual(historyWindow(null, now), {});
  assert.deepEqual(historyWindow({ kind: "quick", key: "1h" }, now), { since: "2026-10-30T11:00:00.000Z", until: undefined });
  assert.deepEqual(
    historyWindow({ kind: "custom", startMS: Date.parse("2026-10-29T08:06:00Z"), endMS: Date.parse("2026-10-29T08:21:00Z") }, now),
    { since: "2026-10-29T08:06:00.000Z", until: "2026-10-29T08:21:00.000Z" },
  );
});

test("a deployment is found by name and chosen from the keyboard", async () => {
  const queries = serve(sample);
  mount();
  await screen.findByText("qwen");
  const box = screen.getByRole("combobox", { name: h.filters.deployment });
  fireEvent.mouseDown(box);
  await waitFor(() => assert.ok(document.querySelector('[role="option"][aria-label="qwen"]')));
  // Searching narrows the options on the server.
  fireEvent.change(box, { target: { value: "lla" } });
  await waitFor(() => assert.ok(!document.querySelector('[role="option"][aria-label="qwen"]')));
  assert.ok(document.querySelector('[role="option"][aria-label="llama"]'));
  // Arrow down to the option and Enter chooses it.
  fireEvent.keyDown(box, { key: "ArrowDown", code: "ArrowDown", keyCode: 40 });
  fireEvent.keyDown(box, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.equal(queries.at(-1)?.deployment_id, 8));
  // Clearing the choice asks for every deployment again.
  fireEvent.mouseEnter(box.closest(".ant-select") as HTMLElement);
  const clear = document.querySelector(".ant-select-clear") as HTMLElement;
  assert.ok(clear, "a chosen deployment can be cleared");
  fireEvent.mouseDown(clear);
  await waitFor(() => assert.equal(queries.at(-1)?.deployment_id, undefined));
});

test("pages follow the entry id as cursor, forward and back", async () => {
  // 25 entries, newest first; each request asks for one more than a page shows.
  const all = Array.from({ length: 25 }, (_, i) => entry(100 - i, { deployment: `dep-${100 - i}`, deployment_id: 100 - i }));
  const queries = serve((query) => all.filter((e) => !query.before || e.id < query.before).slice(0, query.limit));
  const user = mount();
  await screen.findByText("dep-100");
  assert.equal(queries[0].limit, 21);
  assert.equal(screen.getAllByRole("row").length - 1, 20, "a page shows 20 entries");
  assert.equal(screen.queryByText("dep-80"), null);
  // The pager is re-rendered with each page, so it is looked up each time.
  const pager = () => within(screen.getByRole("navigation", { name: h.pagination }));
  assert.equal((pager().getByRole("button", { name: h.newer }) as HTMLButtonElement).disabled, true);

  const older = pager().getByRole("button", { name: h.older });
  older.focus();
  await user.keyboard("{Enter}");
  await screen.findByText("dep-80");
  assert.equal(queries.at(-1)?.before, 81, "the cursor is the last shown entry");
  assert.equal(screen.getAllByRole("row").length - 1, 5);
  assert.ok(pager().getByText(i18n.t("scheduling:history.page", { page: 2 })));
  assert.equal((pager().getByRole("button", { name: h.older }) as HTMLButtonElement).disabled, true, "the last page has no older one");

  await user.click(pager().getByRole("button", { name: h.newer }));
  await screen.findByText("dep-100");
  assert.equal(queries.at(-1)?.before, undefined);

  // A filter starts again from the newest entries.
  await user.click(pager().getByRole("button", { name: h.older }));
  await screen.findByText("dep-80");
  await choose(user, h.category.auto);
  await screen.findByText("dep-100");
  assert.equal(queries.at(-1)?.before, undefined);
});
