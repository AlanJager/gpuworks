import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The summarized earlier time of a busy card on the residency timeline: it is
// drawn inside each deployment's own lane, hatched and coloured by the state
// the server names for the deployment in the slice, left of the dashed line
// where summarizing ends; the tooltip of a slice on hover and keyboard focus;
// the legend's hatch; and the text that replaces "only the latest segments
// are shown".

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const zh = await createI18n(["scheduling", "deployments"], "zh");
const { cleanup, render, screen, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: ResidencyTimeline } = await import("../src/pages/scheduling/ResidencyTimeline.tsx");
const { summaryText } = await import("../src/pages/scheduling/TimelineSummary.tsx");
const { stateColor, lanesOf, CHART_WIDTH } = await import("../src/pages/scheduling/timelineChart.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
type Timeline = import("../src/schedulingApi.ts").SchedulingTimeline;
type BoardNode = import("../src/schedulingApi.ts").BoardNode;
type Summary = import("../src/schedulingApi.ts").TimelineSummary;

const s = locale.scheduling;
const original = { ...schedulingApi };
const FROM = "2026-10-27T12:00:00Z";
const TO = "2026-10-28T12:00:00Z";
const at = (hoursAfterFrom: number) => new Date(Date.parse(FROM) + hoursAfterFrom * 3600_000).toISOString();

const nodes: BoardNode[] = [{
  uuid: "node-a", name: "gpu-a", online: true,
  gpus: [0, 1, 2].map((index) => ({ index, name: "H20", memory_bytes: 1, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 1, state: "free" as const, held_outside: false, switching: false, occupants: [] })),
}];

// Two slices the server computed. deepseek and GLM held the card in the first;
// GLM slept and old-eval (since deleted) was starting in the second. old-eval
// has no raw segments: it held the card only in the summarized time.
const slices: Summary[] = [
  {
    kind: "summary", from: at(0), to: at(10), switches: 85,
    deployments: [
      { deployment_id: 7, deployment: "deepseek", awake_seconds: 720, asleep_seconds: 0, switching_seconds: 60, starting_seconds: 0, state: "awake" },
      { deployment_id: 8, deployment: "GLM", awake_seconds: 540, asleep_seconds: 0, switching_seconds: 0, starting_seconds: 0, state: "awake" },
    ],
  },
  {
    kind: "summary", from: at(10), to: at(12), switches: 1,
    deployments: [
      { deployment_id: 8, deployment: "GLM", awake_seconds: 0, asleep_seconds: 90, switching_seconds: 0, starting_seconds: 30, state: "asleep", level: 1 },
      { deployment_id: 9, deployment: "old-eval", awake_seconds: 0, asleep_seconds: 0, switching_seconds: 0, starting_seconds: 600, state: "starting" },
    ],
  },
];

function timeline(overrides: Partial<Timeline> = {}): Timeline {
  return {
    scope: "node",
    window: { from: FROM, to: TO, hours: 24, key: "24h", seconds: 86400, custom: false },
    windows: [{ key: "15m", seconds: 900 }, { key: "1h", seconds: 3600 }, { key: "6h", seconds: 21600 }, { key: "24h", seconds: 86400 }],
    nodes: [{
      uuid: "node-a", name: "gpu-a",
      gpus: [
        {
          index: 0, truncated: true, summarized_before: at(12), summary: slices,
          segments: [
            { deployment_id: 7, deployment: "deepseek", instance: "i7", state: "awake", from: at(12), to: at(18) },
            { deployment_id: 8, deployment: "GLM", instance: "i8", state: "awake", from: at(18), to: at(24) },
          ],
        },
        { index: 1, truncated: false, segments: [{ deployment_id: 7, deployment: "deepseek", instance: "i7", state: "awake", from: at(0), to: at(24) }] },
        // An older server's answer: truncated, nothing summarized.
        { index: 2, truncated: true, segments: [{ deployment_id: 8, deployment: "GLM", instance: "i8", state: "awake", from: at(20), to: at(24) }] },
      ],
    }],
    deployments: [
      { deployment_id: 8, deployment: "GLM", yield_policy: "timeslice", awake_seconds: 1, awake_share: 0, present_seconds: 1, switches: 0, yielded: 0, yielded_to: 0, wakes: 0, held_requests: 0, cards: [] },
      { deployment_id: 9, deployment: "old-eval", deployment_deleted: true, yield_policy: "", awake_seconds: 0, awake_share: 0, present_seconds: 1, switches: 0, yielded: 0, yielded_to: 0, wakes: 0, held_requests: 0, cards: [] },
    ],
    switches_available: true,
    ...overrides,
  };
}

function mount() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><ResidencyTimeline nodes={nodes} /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

const gpuGroup = (index: number) => screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index }) });
const lane = (card: HTMLElement, deployment: string) => within(card).getByRole("group", { name: i18n.t("scheduling:timeline.laneAria", { deployment }) });
const summaryBlocks = (el: HTMLElement) => within(el).queryAllByRole("img").filter((b) => b.classList.contains("timeline-summary-block"));
const t = i18n.getFixedT(null, "scheduling");

afterEach(() => {
  cleanup();
  Object.assign(schedulingApi, original);
});
after(() => closeChannels());

test("a slice is worded for one deployment from the server's numbers; the states it spent no time in are left out", () => {
  const [first, second] = slices;
  const range = (slice: Summary) => `${formatTime(slice.from)} to ${formatTime(slice.to)}`;
  // The switches are the card's, the seconds the deployment's own.
  assert.equal(summaryText(t, first, first.deployments[0]), `${range(first)} · this card switched 85 times · this deployment serving 12 min · switching 1 min`);
  assert.equal(summaryText(t, first, first.deployments[1]), `${range(first)} · this card switched 85 times · this deployment serving 9 min`);
  // One switch is singular; the same slice words another deployment differently.
  assert.equal(summaryText(t, second, second.deployments[0]), `${range(second)} · this card switched 1 time · this deployment asleep 2 min · starting 30 s`);
  assert.equal(summaryText(t, second, second.deployments[1]), `${range(second)} · this card switched 1 time · this deployment starting 10 min`);
  // A deployment with no whole second in any state is worded by the slice alone.
  assert.equal(summaryText(t, second, { ...second.deployments[1], starting_seconds: 0 }), `${range(second)} · this card switched 1 time`);
  // The same words in Chinese.
  const zhT = zh.i18n.getFixedT(null, "scheduling");
  assert.equal(
    summaryText(zhT, first, { ...first.deployments[0], awake_seconds: 720, asleep_seconds: 780, switching_seconds: 60 }),
    `${formatTime(first.from)} 至 ${formatTime(first.to)} · 本卡切换 85 次 · 本部署服务 12 分钟 · 休眠 13 分钟 · 切换中 1 分钟`,
  );
});

test("lanesOf adds a deployment that held the card only in the summarized time, first as it held the card first", () => {
  const stats = timeline().deployments;
  const lanes = lanesOf(timeline().nodes[0].gpus[0].segments, stats, slices);
  assert.deepEqual(lanes.map((l) => [l.deployment, l.summary.length, l.segments.length]), [["deepseek", 1, 1], ["GLM", 2, 1], ["old-eval", 1, 0]]);
  assert.deepEqual(lanes.map((l) => [l.policy, l.deleted]), [["never", false], ["timeslice", false], ["never", true]]);
  // Without a summary it is what it was.
  assert.deepEqual(lanesOf(timeline().nodes[0].gpus[0].segments, stats).map((l) => [l.deployment, l.summary.length]), [["deepseek", 0], ["GLM", 0]]);
});

test("summarized slices are drawn in each deployment's own lane, hatched, and there is no separate summary lane", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  // One lane per deployment, nothing else: the old "Earlier (summarized)" lane and its group are gone.
  const groups = within(card).getAllByRole("group").map((g) => g.getAttribute("aria-label"));
  assert.deepEqual(groups, ["deepseek", "GLM", "old-eval"].map((deployment) => i18n.t("scheduling:timeline.laneAria", { deployment })));
  assert.equal(card.querySelectorAll(".timeline-summary-lane").length, 0);
  assert.equal(within(card).queryByText("Earlier (summarized)"), null);
  assert.equal(within(card).queryByRole("list"), null);
  // Each deployment's lane holds its own slices and its own raw segments.
  const deepseek = lane(card, "deepseek");
  const glm = lane(card, "GLM");
  const oldEval = lane(card, "old-eval");
  assert.deepEqual([deepseek, glm, oldEval].map((l) => summaryBlocks(l).length), [1, 2, 1]);
  assert.deepEqual([deepseek, glm, oldEval].map((l) => within(l).getAllByRole("img").length - summaryBlocks(l).length), [1, 1, 0]);
  // To scale on the whole window: deepseek's slice is the first 10 h of 24, GLM's second is hours 10 to 12.
  const widths = (el: HTMLElement) => summaryBlocks(el).map((b) => [Math.round(parseFloat(b.getAttribute("x") ?? "0")), Math.round(parseFloat(b.getAttribute("width") ?? "0"))]);
  assert.deepEqual(widths(deepseek), [[0, Math.round((10 / 24) * CHART_WIDTH)]]);
  assert.deepEqual(widths(glm), [[0, Math.round((10 / 24) * CHART_WIDTH)], [Math.round((10 / 24) * CHART_WIDTH), Math.round((2 / 24) * CHART_WIDTH)]]);
  // The raw segment of deepseek begins where its summary ends, not before.
  const raw = within(deepseek).getAllByRole("img").find((b) => !b.classList.contains("timeline-summary-block"));
  assert.equal(Math.round(parseFloat(raw?.getAttribute("x") ?? "0")), CHART_WIDTH / 2);
  // A pattern per lane with slices, and a hatch over each slice, which is no extra thing to tab to.
  assert.deepEqual([deepseek, glm, oldEval].map((l) => l.querySelectorAll("pattern").length), [1, 1, 1]);
  assert.deepEqual([deepseek, glm, oldEval].map((l) => l.querySelectorAll('rect[fill^="url(#"][aria-hidden="true"]').length), [1, 2, 1]);
  const ids = new Set([deepseek, glm, oldEval].map((l) => l.querySelector("pattern")?.id));
  assert.equal(ids.size, 3, "each lane's hatch has an id of its own");
});

test("a slice is coloured by the server's state for the deployment, with the colours of raw segments", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  const fills = (deployment: string) => summaryBlocks(lane(card, deployment)).map((b) => b.getAttribute("fill"));
  assert.deepEqual(fills("deepseek"), [stateColor.awake]);
  assert.deepEqual(fills("GLM"), [stateColor.awake, stateColor.asleep]);
  assert.deepEqual(fills("old-eval"), [stateColor.starting]);
  // Nothing but the server's state decides it: a deployment whose seconds are mostly awake but whose state says switching is drawn switching.
  cleanup();
  const odd = timeline();
  odd.nodes[0].gpus[0].summary = [{ ...slices[0], deployments: [{ ...slices[0].deployments[0], state: "switching" }] }];
  schedulingApi.timeline = async () => odd;
  mount();
  const again = await gpuGroup(0);
  assert.deepEqual(summaryBlocks(lane(again, "deepseek")).map((b) => b.getAttribute("fill")), [stateColor.switching]);
});

test("a deployment that held the card only in the summarized time gets a lane like the others", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  // old-eval has been deleted: its label is plain text, not a link, as in any lane.
  assert.ok(within(card).getByText(i18n.t("scheduling:history.deleted", { name: "old-eval" })));
  assert.equal(within(card).queryByRole("link", { name: "old-eval" }), null);
  // GLM, from the stats, has its link and its yield policy; deepseek is a resident one.
  assert.equal(within(card).getByRole("link", { name: "GLM" }).getAttribute("href"), "/deployments?deployment=8");
  assert.equal(within(card).getAllByText(i18n.t("deployments:yieldPolicy.policy.name.timeslice")).length, 1);
  assert.equal(within(card).getAllByText(i18n.t("deployments:yieldPolicy.policy.name.never")).length, 1);
  // Its one slice is a block of its own with its words.
  const [block] = summaryBlocks(lane(card, "old-eval"));
  assert.equal(block.getAttribute("aria-label"), summaryText(t, slices[1], slices[1].deployments[1]));
  // A live deployment that held the card only in the summary has a link too.
  cleanup();
  const live = timeline({ deployments: [] });
  schedulingApi.timeline = async () => live;
  mount();
  const liveCard = await gpuGroup(0);
  assert.equal(within(liveCard).getByRole("link", { name: "old-eval" }).getAttribute("href"), "/deployments?deployment=9");
});

test("a slice describes itself to assistive tech and shows its tooltip on hover and on keyboard focus, in its own lane", async () => {
  schedulingApi.timeline = async () => timeline();
  const user = mount();
  const card = await gpuGroup(0);
  const [first] = summaryBlocks(lane(card, "deepseek"));
  const expected = summaryText(t, slices[0], slices[0].deployments[0]);
  assert.equal(first.getAttribute("aria-label"), expected);
  assert.equal(first.getAttribute("role"), "img");
  assert.equal(first.getAttribute("tabindex"), "0");
  assert.equal(screen.queryByRole("tooltip"), null);
  // Hover: the tooltip is in the lane's track, and shows only the states the deployment spent time in.
  fireEvent.mouseEnter(first);
  const tip = screen.getByRole("tooltip");
  assert.equal(tip.textContent, expected);
  assert.ok(lane(card, "deepseek").closest(".timeline-track")?.contains(tip));
  assert.ok(tip.textContent?.includes("this card switched 85 times"));
  assert.ok(tip.textContent?.includes("serving 12 min") && tip.textContent?.includes("switching 1 min"));
  assert.ok(!tip.textContent?.includes("asleep") && !tip.textContent?.includes("starting"));
  fireEvent.mouseLeave(first);
  assert.equal(screen.queryByRole("tooltip"), null);
  // The same slice words each deployment differently: GLM's second slice shows asleep and starting, not serving.
  const [, glmLate] = summaryBlocks(lane(card, "GLM"));
  fireEvent.focus(glmLate);
  assert.ok(screen.getByRole("tooltip").textContent?.includes("this deployment asleep 2 min · starting 30 s"));
  assert.ok(!screen.getByRole("tooltip").textContent?.includes("serving"));
  fireEvent.blur(glmLate);
  // The keyboard: tab to the slice, the tooltip shows; Escape hides it; the next tab moves on to the lane's raw segment.
  await user.tab();
  while (document.activeElement !== first) {
    await user.tab();
    if (document.activeElement === document.body) assert.fail("the first summary never took focus");
  }
  assert.equal(screen.getByRole("tooltip").textContent, expected);
  await user.keyboard("{Escape}");
  assert.equal(screen.queryByRole("tooltip"), null);
  await user.tab();
  assert.equal(screen.getByRole("tooltip").textContent, `deepseek · ${s.timeline.state.awake} · ${formatTime(at(12))} to ${formatTime(at(18))}`);
});

test("the legend keeps the state colours and adds the hatch for summarized time", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  await gpuGroup(0);
  const legend = await screen.findByRole("list", { name: s.timeline.legend });
  await within(legend).findByText(s.timeline.summary.legend);
  const items = within(legend).getAllByRole("listitem").map((li) => li.textContent);
  assert.deepEqual(items, [s.timeline.state.awake, s.timeline.state.asleep, s.timeline.state.switching, s.timeline.state.starting, "Summarized (earlier)"]);
  // Only the last swatch is hatched; there is no per-deployment key any more.
  const swatches = [...legend.querySelectorAll<HTMLElement>(".scheduling-swatch")];
  assert.deepEqual(swatches.map((sw) => sw.classList.contains("timeline-summary-swatch")), [false, false, false, false, true]);
  assert.equal(screen.queryByRole("list", { name: /main deployment/i }), null);
});

test("a line marks where summarizing ends on every lane of the card, and on no other card", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  for (const deployment of ["deepseek", "GLM", "old-eval"]) {
    const lines = lane(card, deployment).querySelectorAll("line[stroke-dasharray]");
    assert.equal(lines.length, 1, deployment);
    assert.equal(lines[0].getAttribute("x1"), String(CHART_WIDTH / 2));
    assert.equal(lines[0].getAttribute("aria-hidden"), "true");
  }
  assert.equal(card.querySelectorAll("line[stroke-dasharray]").length, 3);
  for (const index of [1, 2]) assert.equal((await gpuGroup(index)).querySelectorAll("line[stroke-dasharray]").length, 0);
});

test("the card says its earlier time is summarized, not that only the latest segments are shown", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  assert.ok(within(card).getByText(i18n.t("scheduling:timeline.summarized", { time: formatTime(at(12)) })));
  assert.equal(within(card).queryByText(s.timeline.truncated), null);
  assert.equal(screen.queryByText("Only the latest segments are shown"), null);
  // An answer that truncates without summarizing says so in its own words, once.
  const plain = await gpuGroup(2);
  assert.ok(within(plain).getByText(s.timeline.truncated));
  assert.equal(within(plain).queryByText(/Summarized before/), null);
});

test("a card with nothing summarized is drawn as it was: its lanes have no hatch, line or summary block", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  for (const index of [1, 2]) {
    const card = await gpuGroup(index);
    assert.equal(card.querySelectorAll("pattern").length, 0);
    assert.equal(card.querySelectorAll("line[stroke-dasharray]").length, 0);
    assert.equal(card.querySelectorAll(".timeline-summary-block").length, 0);
    assert.equal(within(card).getAllByRole("group").length, 1, "one lane");
    assert.equal(within(card).getAllByRole("img").length, 1, "one raw segment");
  }
  // With no card summarized at all, the legend is the four states only.
  cleanup();
  const plain = timeline();
  plain.nodes[0].gpus = plain.nodes[0].gpus.slice(1);
  schedulingApi.timeline = async () => plain;
  mount();
  await gpuGroup(1);
  const legend = screen.getByRole("list", { name: s.timeline.legend });
  assert.equal(within(legend).getAllByRole("listitem").length, 4);
  assert.equal(within(legend).queryByText(s.timeline.summary.legend), null);
});

test("the loading and error states are unaffected by summaries", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  schedulingApi.timeline = async () => { await gate; return timeline(); };
  mount();
  assert.ok(screen.getByText(s.timeline.loading));
  release();
  await gpuGroup(0);
  assert.equal(screen.queryByText(s.timeline.loading), null);
  cleanup();
  schedulingApi.timeline = async () => { throw new Error("timeline unavailable"); };
  mount();
  assert.ok(await screen.findByText(s.timeline.loadFailed));
});

test("a card with summaries and no raw segments is not reported as empty, and its lanes are the summarized deployments", async () => {
  const answer = timeline();
  answer.nodes[0].gpus = [{ index: 0, truncated: true, summarized_before: at(12), summary: slices, segments: [] }];
  schedulingApi.timeline = async () => answer;
  mount();
  const card = await gpuGroup(0);
  assert.equal(within(card).queryByText(s.timeline.noneOnCard), null);
  assert.equal(screen.queryByText(i18n.t("scheduling:timeline.noneInWindow", { span: i18n.t("scheduling:timeline.hours", { hours: 24 }) })), null);
  assert.deepEqual(within(card).getAllByRole("group").map((g) => g.getAttribute("aria-label")), ["deepseek", "GLM", "old-eval"].map((deployment) => i18n.t("scheduling:timeline.laneAria", { deployment })));
  assert.equal(card.querySelectorAll(".timeline-summary-block").length, 4);
  assert.equal(card.querySelectorAll("line[stroke-dasharray]").length, 3);
});
