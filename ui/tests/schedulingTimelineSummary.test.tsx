import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The summarized earlier time of a busy card on the residency timeline: the
// hatched lane the server's slices are drawn in, the line where summarizing
// ends, the tooltip of a slice on hover and keyboard focus, and the text that
// replaces "only the latest segments are shown".

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, render, screen, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: ResidencyTimeline } = await import("../src/pages/scheduling/ResidencyTimeline.tsx");
const { summaryText } = await import("../src/pages/scheduling/TimelineSummary.tsx");
const { deploymentColor, CHART_WIDTH } = await import("../src/pages/scheduling/timelineChart.ts");
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

// Two slices the server computed: deepseek led the first, nobody served in the second.
const slices: Summary[] = [
  {
    kind: "summary", from: at(0), to: at(10), switches: 85, dominant_deployment_id: 7, dominant_deployment: "deepseek",
    deployments: [
      { deployment_id: 7, deployment: "deepseek", awake_seconds: 720, asleep_seconds: 0, switching_seconds: 60, starting_seconds: 0 },
      { deployment_id: 8, deployment: "GLM", awake_seconds: 540, asleep_seconds: 0, switching_seconds: 0, starting_seconds: 0 },
    ],
  },
  {
    kind: "summary", from: at(10), to: at(12), switches: 1,
    deployments: [{ deployment_id: 8, deployment: "GLM", awake_seconds: 0, asleep_seconds: 90, switching_seconds: 0, starting_seconds: 30 }],
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
    deployments: [],
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

afterEach(() => {
  cleanup();
  Object.assign(schedulingApi, original);
});
after(() => closeChannels());

test("a slice is worded from the server's numbers: time, switches, and what each deployment spent", () => {
  const t = i18n.getFixedT(null, "scheduling");
  assert.equal(
    summaryText(t, slices[0]),
    `${formatTime(at(0))} to ${formatTime(at(10))} · 85 switches · deepseek serving 12 min, switching 1 min · GLM serving 9 min`,
  );
  // A state it spent no time in is left out; one switch is singular.
  assert.equal(
    summaryText(t, slices[1]),
    `${formatTime(at(10))} to ${formatTime(at(12))} · 1 switch · GLM asleep 2 min, starting 30 s`,
  );
});

test("the hatched summary lane draws a block per slice, coloured by the server's dominant deployment", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  const lane = within(card).getByRole("group", { name: i18n.t("scheduling:timeline.summary.laneAria", { time: formatTime(at(12)) }) });
  const blocks = within(lane).getAllByRole("img").filter((el) => el.classList.contains("timeline-summary-block"));
  assert.equal(blocks.length, 2);
  assert.deepEqual(blocks.map((b) => b.getAttribute("fill")), [deploymentColor(7), deploymentColor(undefined)]);
  assert.notEqual(deploymentColor(7), deploymentColor(8));
  // To scale: 10 h and 2 h of 24 h.
  assert.deepEqual(blocks.map((b) => Math.round(parseFloat(b.getAttribute("width") ?? "0"))), [Math.round((10 / 24) * CHART_WIDTH), Math.round((2 / 24) * CHART_WIDTH)]);
  // The hatch is drawn over each block, and is no extra thing to tab to.
  assert.equal(lane.querySelectorAll("pattern").length, 1);
  assert.equal(lane.querySelectorAll('rect[fill^="url(#"]').length, 2);
  // The key names the dominant deployments, and the grey for a slice nobody served in.
  const key = within(card).getByRole("list", { name: i18n.t("scheduling:timeline.summary.keyAria") });
  assert.ok(within(key).getByText("deepseek"));
  assert.ok(within(key).getByText(i18n.t("scheduling:timeline.summary.noDominant")));
});

test("a slice names itself to assistive tech and shows its tooltip on hover and on keyboard focus", async () => {
  schedulingApi.timeline = async () => timeline();
  const user = mount();
  const card = await gpuGroup(0);
  const t = i18n.getFixedT(null, "scheduling");
  const first = within(card).getByRole("img", { name: summaryText(t, slices[0]) });
  assert.equal(first.getAttribute("tabindex"), "0");
  assert.equal(screen.queryByRole("tooltip"), null);
  fireEvent.mouseEnter(first);
  assert.equal(screen.getByRole("tooltip").textContent, summaryText(t, slices[0]));
  fireEvent.mouseLeave(first);
  assert.equal(screen.queryByRole("tooltip"), null);
  // The keyboard: tab to the slice, the tooltip shows; Escape hides it; the next tab moves to the next slice.
  await user.tab();
  while (document.activeElement !== first) {
    await user.tab();
    if (document.activeElement === document.body) assert.fail("the first summary never took focus");
  }
  assert.equal(screen.getByRole("tooltip").textContent, summaryText(t, slices[0]));
  assert.ok(screen.getByRole("tooltip").textContent?.includes("85 switches"));
  await user.keyboard("{Escape}");
  assert.equal(screen.queryByRole("tooltip"), null);
  await user.tab();
  assert.equal(screen.getByRole("tooltip").textContent, summaryText(t, slices[1]));
  await user.tab();
  assert.equal(screen.queryByRole("tooltip"), null);
});

test("a line marks where summarizing ends, on the summary lane and on the card's lanes", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(0);
  const marker = within(card).getByRole("img", { name: i18n.t("scheduling:timeline.summary.boundaryAria", { time: formatTime(at(12)) }) });
  assert.equal(marker.querySelector("line")?.getAttribute("x1"), String(CHART_WIDTH / 2));
  // One on each of the two deployment lanes as well, hidden from assistive tech.
  const dashed = card.querySelectorAll("line[stroke-dasharray]");
  assert.equal(dashed.length, 3);
  assert.ok([...dashed].every((line) => line.getAttribute("x1") === String(CHART_WIDTH / 2)));
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
  assert.equal(within(plain).queryByRole("group", { name: /Holding of this card/ }), null);
});

test("a card with nothing summarized draws no summary lane or line", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const card = await gpuGroup(1);
  assert.equal(within(card).queryByRole("group", { name: /Holding of this card/ }), null);
  assert.equal(within(card).queryByRole("list", { name: i18n.t("scheduling:timeline.summary.keyAria") }), null);
  assert.equal(card.querySelectorAll("line[stroke-dasharray]").length, 0);
  assert.equal(within(card).queryByText(/Summarized before/), null);
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

test("a card with summaries and no raw segments is not reported as empty", async () => {
  // Nothing held the card in the raw part: the summary lane is the card's whole story, not "no instance held it".
  const answer = timeline();
  answer.nodes[0].gpus[0] = { index: 0, truncated: true, summarized_before: at(12), summary: slices, segments: [] };
  schedulingApi.timeline = async () => answer;
  mount();
  const card = await gpuGroup(0);
  assert.equal(within(card).queryByText(s.timeline.noneOnCard), null);
  assert.equal(within(card).getAllByRole("img").filter((el) => el.classList.contains("timeline-summary-block")).length, 2);
});
