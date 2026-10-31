import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The residency timeline card: its loading / error / empty / success states,
// the lanes drawn to scale with their ticks and legend, the tooltip a hovered
// or focused block shows, the window selector and the sharing table.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["scheduling", "deployments", "deploy"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { schedulingApi } = await import("../src/schedulingApi.ts");
const { default: ResidencyTimeline } = await import("../src/pages/scheduling/ResidencyTimeline.tsx");
const { blockOf, ticksOf, lanesOf, CHART_WIDTH } = await import("../src/pages/scheduling/timelineChart.ts");
const { colors } = await import("../src/theme/tokens.ts");
type Timeline = import("../src/schedulingApi.ts").SchedulingTimeline;
type BoardNode = import("../src/schedulingApi.ts").BoardNode;

const s = locale.scheduling;
const original = { ...schedulingApi };
const FROM = "2026-10-28T06:00:00Z";
const TO = "2026-10-28T12:00:00Z";
const at = (hoursAfterFrom: number) => new Date(Date.parse(FROM) + hoursAfterFrom * 3600_000).toISOString();

const nodes: BoardNode[] = [{
  uuid: "node-a", name: "gpu-a", online: true,
  gpus: [
    { index: 0, name: "RTX 8000", memory_bytes: 1, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 1, state: "free", held_outside: false, switching: false, occupants: [] },
    { index: 1, name: "RTX 8000", memory_bytes: 1, awake_bytes: 0, sleeping_bytes: 0, free_bytes: 1, state: "free", held_outside: false, switching: false, occupants: [] },
  ],
}];

function timeline(overrides: Partial<Timeline> = {}): Timeline {
  return {
    window: { from: FROM, to: TO, hours: 6 },
    nodes: [{
      uuid: "node-a", name: "gpu-a",
      gpus: [
        {
          index: 0, truncated: false,
          segments: [
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "awake", from: at(0), to: at(2) },
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "switching", from: at(2), to: at(2.5) },
            { deployment_id: 7, deployment: "qwen", instance: "i7", state: "asleep", level: 2, from: at(2.5), to: at(4) },
            { deployment_id: 8, deployment: "llama", instance: "i8", state: "awake", from: at(4), to: at(6) },
          ],
        },
        { index: 1, truncated: true, segments: [{ deployment_id: 9, deployment: "always-on", instance: "i9", state: "starting", from: at(5), to: at(6) }] },
      ],
    }],
    deployments: [
      { deployment_id: 7, deployment: "qwen", yield_policy: "timeslice", awake_seconds: 7200, awake_share: 0.3333, switches: 3, yielded: 1, yielded_to: 2, wakes: 2, held_requests: 4, wait_p50_ms: 4200, wait_p95_ms: 31000, cards: [{ node_id: 1, node: "gpu-a", gpus: [0, 1] }, { node_id: 9, node: "", gpus: [2] }] },
      { deployment_id: 8, deployment: "llama", yield_policy: "anytime", awake_seconds: 30, awake_share: 0.0014, switches: 0, yielded: 0, yielded_to: 0, wakes: 0, held_requests: 0, cards: [] },
    ],
    ...overrides,
  };
}

function mount(list: BoardNode[] = nodes) {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter><ResidencyTimeline nodes={list} /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup();
}

afterEach(() => {
  cleanup();
  Object.assign(schedulingApi, original);
});
after(() => closeChannels());

test("the chart geometry is to scale: a block's width is its share of the window and ticks sit on the hours", () => {
  const span = { fromMs: Date.parse(FROM), toMs: Date.parse(TO) };
  const block = blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(1.5), to: at(4.5) }, span);
  assert.equal(block.x, 250);
  assert.equal(block.width, 500);
  // A stretch before the window starts at its edge; one shorter than a pixel is still drawn.
  assert.equal(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(-3), to: at(1) }, span).x, 0);
  assert.ok(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(3), to: at(3.0001) }, span).width >= 1);
  assert.equal(blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(5.99), to: at(6) }, span).x + blockOf({ deployment_id: 1, deployment: "d", instance: "i", state: "awake", from: at(5.99), to: at(6) }, span).width, CHART_WIDTH);
  const six = ticksOf(span, 6);
  assert.ok(six.length >= 5 && six.length <= 7, `${six.length} hourly ticks in 6 h`);
  for (const tick of six) {
    assert.equal(new Date(tick.ms).getMinutes(), 0);
    assert.equal(tick.x, ((tick.ms - span.fromMs) / (span.toMs - span.fromMs)) * CHART_WIDTH);
  }
  const wide = ticksOf({ fromMs: Date.parse(FROM), toMs: Date.parse(FROM) + 24 * 3600_000 }, 24);
  assert.ok(wide.length >= 5 && wide.length <= 7, `${wide.length} four-hour ticks in 24 h`);
  assert.ok(wide.every((tick) => new Date(tick.ms).getHours() % 4 === 0));
});

test("lanes are one per deployment in the order they first held the card; unlisted deployments are resident", () => {
  const lanes = lanesOf(timeline().nodes[0].gpus[0].segments, timeline().deployments);
  assert.deepEqual(lanes.map((lane) => [lane.deployment, lane.policy, lane.segments.length]), [["qwen", "timeslice", 3], ["llama", "anytime", 1]]);
  assert.equal(lanesOf(timeline().nodes[0].gpus[1].segments, timeline().deployments)[0].policy, "never");
});

test("a loading status shows until the timeline answers", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  schedulingApi.timeline = async () => { await gate; return timeline(); };
  mount();
  assert.ok(screen.getByRole("status"));
  assert.ok(screen.getByText(s.timeline.loading));
  release();
  await screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  assert.equal(screen.queryByText(s.timeline.loading), null);
});

test("a failed first load shows the error and retries on request", async () => {
  let calls = 0;
  schedulingApi.timeline = async () => {
    calls += 1;
    if (calls === 1) throw new Error("timeline unavailable");
    return timeline();
  };
  const user = mount();
  assert.ok(await screen.findByText(s.timeline.loadFailed));
  assert.ok(screen.getByText("timeline unavailable"));
  await user.click(screen.getByRole("button", { name: s.page.retry }));
  await screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  assert.equal(screen.queryByText(s.timeline.loadFailed), null);
});

test("with no nodes on the page there is nothing to ask for", async () => {
  let asked = 0;
  schedulingApi.timeline = async () => { asked += 1; return timeline(); };
  mount([]);
  assert.ok(screen.getByText(s.timeline.noNodes));
  assert.equal(asked, 0);
});

test("a window nobody held a card in says so, and an empty sharing table says so", async () => {
  schedulingApi.timeline = async () => timeline({
    nodes: [{ uuid: "node-a", name: "gpu-a", gpus: [{ index: 0, truncated: false, segments: [] }] }],
    deployments: [],
  });
  mount();
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.noneInWindow", { hours: 6 })));
  assert.ok(screen.getByText(s.timeline.noneOnCard));
  assert.ok(screen.getByText(s.timeline.sharingEmpty));
});

test("each card is lanes of blocks coloured by state from the theme, with a legend and hour ticks", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  const gpu0 = within(await screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
  assert.ok(gpu0.getByText(i18n.t("scheduling:nodes.gpuLabel", { index: 0, name: "RTX 8000" })));
  const qwen = within(gpu0.getByRole("group", { name: i18n.t("scheduling:timeline.laneAria", { deployment: "qwen" }) }));
  const blocks = qwen.getAllByRole("img");
  assert.equal(blocks.length, 3);
  assert.deepEqual(blocks.map((b) => b.getAttribute("fill")), [colors.chartPrimary, colors.chartSecondary, colors.chartQuaternary]);
  // 2 h of 6 h, then half an hour, then an hour and a half.
  assert.deepEqual(blocks.map((b) => Math.round(parseFloat(b.getAttribute("width") ?? "0"))), [333, 83, 250]);
  assert.deepEqual(blocks.map((b) => Math.round(parseFloat(b.getAttribute("x") ?? "0"))), [0, 333, 417]);
  // The deployment's yield policy labels its lane.
  assert.ok(gpu0.getAllByText(i18n.t("deployments:yieldPolicy.policy.name.timeslice")).length >= 1);
  assert.ok(gpu0.getByRole("link", { name: "llama" }));
  const legend = screen.getByRole("list", { name: s.timeline.legend });
  for (const label of [s.timeline.state.awake, s.timeline.state.asleep, s.timeline.state.switching, s.timeline.state.starting]) assert.ok(within(legend).getByText(label), label);
  const ticks = document.querySelectorAll(".timeline-axis-ticks > span");
  assert.ok(ticks.length >= 5, `${ticks.length} ticks`);
  // A truncated card says so; one that is not does not.
  assert.ok(screen.getByText(s.timeline.truncated));
  assert.equal(screen.getAllByText(s.timeline.truncated).length, 1);
});

test("a block names itself to assistive tech and shows a tooltip on hover and on keyboard focus", async () => {
  schedulingApi.timeline = async () => timeline();
  const user = mount();
  const sleeping = await screen.findByRole("img", { name: new RegExp(`qwen · ${i18n.t("scheduling:timeline.state.asleepLevel", { level: 2 })}`) });
  assert.equal(sleeping.getAttribute("tabindex"), "0");
  assert.equal(screen.queryByRole("tooltip"), null);
  // Hover.
  fireEvent.mouseEnter(sleeping);
  const tip = screen.getByRole("tooltip");
  assert.equal(tip.textContent, sleeping.getAttribute("aria-label"));
  assert.ok(tip.textContent?.includes("qwen"));
  fireEvent.mouseLeave(sleeping);
  assert.equal(screen.queryByRole("tooltip"), null);
  // The keyboard: tab to a block, the tooltip shows, Escape and blur hide it.
  const first = screen.getAllByRole("img", { name: /qwen/ })[0];
  await user.tab();
  while (document.activeElement !== first) {
    await user.tab();
    if (document.activeElement === document.body) assert.fail("the first block never took focus");
  }
  assert.ok(screen.getByRole("tooltip").textContent?.includes(i18n.t("scheduling:timeline.state.awake")));
  await user.keyboard("{Escape}");
  assert.equal(screen.queryByRole("tooltip"), null);
  await user.tab();
  assert.ok(screen.getByRole("tooltip").textContent?.includes(i18n.t("scheduling:timeline.state.switching")));
  await user.tab({ shift: true });
  await user.tab({ shift: true });
  assert.equal(screen.queryByRole("tooltip"), null);
});

test("the window selector asks for 24 hours and keeps showing the card meanwhile", async () => {
  const asked: unknown[] = [];
  schedulingApi.timeline = async (query) => {
    asked.push(query);
    return timeline({ window: { from: FROM, to: TO, hours: query.hours } });
  };
  const user = mount();
  await screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  assert.deepEqual(asked, [{ nodes: ["node-a"], hours: 6 }]);
  const selector = screen.getByRole("radiogroup", { name: s.timeline.window });
  assert.ok(within(selector).getByLabelText(i18n.t("scheduling:timeline.hours", { hours: 6 })));
  await user.click(within(selector).getByText(i18n.t("scheduling:timeline.hours", { hours: 24 })));
  await waitFor(() => assert.deepEqual(asked.at(-1), { nodes: ["node-a"], hours: 24 }));
  assert.ok(screen.getByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
});

test("the sharing table lists the yield policy, awake time, switches, yields and the held requests' waits", async () => {
  schedulingApi.timeline = async () => timeline();
  mount();
  await screen.findByText(s.timeline.sharingTitle);
  const table = within(screen.getByRole("table"));
  for (const heading of Object.values(s.timeline.columns)) assert.ok(table.getAllByText(heading).length >= 1, heading);
  const qwen = table.getByRole("link", { name: "qwen" }).closest("tr") as HTMLElement;
  const cells = within(qwen);
  assert.ok(cells.getByText(i18n.t("deployments:yieldPolicy.policy.name.timeslice")));
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.awakeShare", { span: i18n.t("deployments:duration.hoursMinutes", { hours: 2, minutes: 0 }), share: 33.3 })));
  // Where it is now: each node with its GPUs, a node no longer known by id.
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.cardsOnNode", { node: "gpu-a", gpus: "0, 1" })));
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.cardsOnNode", { node: "#9", gpus: "2" })));
  assert.ok(cells.getByText("3")); // switches
  assert.ok(cells.getByText("4")); // held requests
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.wait.seconds", { value: "4.2" })));
  assert.ok(cells.getByText(i18n.t("scheduling:timeline.wait.seconds", { value: "31" })));
  // No held request, no wait figure: a dash, not a zero.
  const llama = within(table.getByRole("link", { name: "llama" }).closest("tr") as HTMLElement);
  // Not placed anywhere, and no wait figures: dashes, not zeros.
  assert.equal(llama.getAllByText("—").length, 3);
  assert.ok(llama.getByText(i18n.t("scheduling:timeline.awakeShare", { span: i18n.t("deployments:duration.seconds", { value: 30 }), share: 0.1 })));
});

test("a failed refresh keeps the last timeline and warns", async () => {
  let calls = 0;
  schedulingApi.timeline = async () => {
    calls += 1;
    if (calls > 1) throw new Error("boom");
    return timeline();
  };
  const user = mount();
  await screen.findByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) });
  await user.click(within(screen.getByRole("radiogroup", { name: s.timeline.window })).getByText(i18n.t("scheduling:timeline.hours", { hours: 24 })));
  assert.ok(await screen.findByText(i18n.t("scheduling:timeline.refreshFailed", { detail: "boom" })));
  assert.ok(screen.getByRole("group", { name: i18n.t("scheduling:timeline.gpuAria", { node: "gpu-a", index: 0 }) }));
});
