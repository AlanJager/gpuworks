import assert from "node:assert/strict";
import test from "node:test";
import { buildTimeSeriesOption, metricSeriesID } from "../src/components/TimeSeriesChart.tsx";

test("time-series ids stay unique when separate metrics have identical labels", () => {
  const labels = { engine: "vllm", instance: "instance-1", node: "node-1" };

  assert.notEqual(metricSeriesID(labels, 0), metricSeriesID(labels, 1));
  assert.equal(metricSeriesID(labels, 0), metricSeriesID({ ...labels }, 0));
});

test("charts carry no dataZoom: the drawer scroll mis-triggers it", () => {
  const option = buildTimeSeriesOption(
    [{ labels: { instance: "i-1" }, points: [[1, 2], [2, 3]] }],
    { valueFmt: (v) => String(v), seriesName: () => "i-1" },
  );
  // The slider was removed deliberately and wheel zoom with it; the visible
  // window is chosen by the time-range control above the chart instead.
  assert.equal(option.dataZoom, undefined);
  // A single line is named by the chart's title: no legend, and the bottom
  // strip only hosts the x-axis labels.
  assert.equal((option.legend as { show: boolean }).show, false);
  assert.equal((option.grid as { bottom: number }).bottom, 30);
  assert.equal(option.series.length, 1);
  assert.deepEqual((option.series[0] as { data: unknown }).data, [[1, 2], [2, 3]]);
});

test("several lines get a legend below the x-axis labels, and the unit sits above the y-axis", () => {
  const option = buildTimeSeriesOption(
    [{ labels: { gpu: "0" }, points: [[1, 2]] }, { labels: { gpu: "1" }, points: [[1, 3]] }],
    { valueFmt: (v) => String(v), unit: "GiB" },
  );
  // The bottom strip hosts x-axis labels and the legend as separate rows.
  assert.equal((option.legend as { show: boolean; bottom: number }).show, true);
  assert.equal((option.legend as { bottom: number }).bottom, 2);
  assert.equal((option.grid as { bottom: number }).bottom, 52);
  assert.deepEqual((option.legend as { data: string[] }).data, ["GPU 0", "GPU 1"]);
  assert.equal((option.yAxis as { name: string }).name, "GiB");
});

test("default names never print a label's key", async () => {
  const { defaultSeriesName } = await import("../src/components/TimeSeriesChart.tsx");
  assert.equal(defaultSeriesName({ metric: "Running requests" }, 0), "Running requests");
  assert.equal(defaultSeriesName({ node: "b5070775aaaabbbbcccc", gpu: "1", gpu_uuid: "GPU-e330d2f2-9085" }, 0), "GPU 1");
  assert.equal(defaultSeriesName({ node: "b5070775aaaabbbbcccc", instance: "b89afdcc8f77b4df" }, 0), "b89afdcc…");
  assert.equal(defaultSeriesName({ node: "n1" }, 0), "n1");
  assert.equal(defaultSeriesName({ node: "b5070775aaaabbbbcccc", node_name: "gpu3", gpu: "1" }, 0), "gpu3 · GPU 1");
  assert.equal(defaultSeriesName({ node: "b5070775aaaabbbbcccc", node_name: "gpu3" }, 0), "gpu3");
  assert.equal(defaultSeriesName({}, 2), "series 3");
});

test("empty series still produce a valid option with an empty legend", () => {
  const option = buildTimeSeriesOption([], { valueFmt: (v) => String(v) });
  assert.equal(option.series.length, 0);
  assert.deepEqual((option.legend as { data: string[] }).data, []);
});

test("a pinned value axis holds its range; an unpinned one fits the data", () => {
  const data = [{ labels: { gpu: "1" }, points: [[1, 0], [2, 0]] as Array<[number, number]> }];
  const pinned = buildTimeSeriesOption(data, { valueFmt: String, yRange: [0, 100] }).yAxis as { min?: number; max?: number; scale: boolean };
  assert.deepEqual([pinned.min, pinned.max, pinned.scale, (pinned as { interval?: number }).interval], [0, 100, false, 20]);
  const fitted = buildTimeSeriesOption(data, { valueFmt: String }).yAxis as { min?: number; max?: number; scale: boolean };
  assert.deepEqual([fitted.min, fitted.max, fitted.scale], [undefined, undefined, true]);
});

test("counts keep whole-number ticks and small rates keep their decimals", async () => {
  const { autoValueFmt } = await import("../src/components/TimeSeriesChart.tsx");
  const data = [{ labels: { metric: "running" }, points: [[1, 0], [2, 0]] as Array<[number, number]> }];
  assert.equal((buildTimeSeriesOption(data, { valueFmt: String, integer: true }).yAxis as { minInterval?: number }).minInterval, 1);
  assert.equal((buildTimeSeriesOption(data, { valueFmt: String }).yAxis as { minInterval?: number }).minInterval, undefined);
  assert.deepEqual([0.02, 0.06, 0.1, 1.25, 12.34, 456.7, 3].map(autoValueFmt), ["0.02", "0.06", "0.10", "1.3", "12.3", "457", "3"]);
});

test("time labels stay a fifth of the span apart, pass after pass", async () => {
  const { spacedTimeLabels } = await import("../src/components/TimeSeriesChart.tsx");
  const minute = 60_000;
  const start = new Date(2026, 9, 18, 4, 44).getTime();
  const ticks = Array.from({ length: 9 }, (_, i) => start + i * 2 * minute); // a tick every 2 minutes over 16 minutes
  const format = spacedTimeLabels(16 * minute);
  const pass = () => ticks.map(format);
  const first = pass();
  assert.deepEqual(first, ["04:44", "", "04:48", "", "04:52", "", "04:56", "", "05:00"]);
  assert.deepEqual(pass(), first, "a second layout pass labels the same ticks");
  assert.equal(spacedTimeLabels(3 * 86_400_000)(new Date(2026, 9, 18, 4, 44).getTime()), "10-18 04:44");
  const data = [{ labels: { gpu: "1" }, points: [[start, 1], [start + 16 * minute, 2]] as Array<[number, number]> }];
  assert.equal(typeof (buildTimeSeriesOption(data, { valueFmt: String }).xAxis as { axisLabel: { formatter?: unknown } }).axisLabel.formatter, "function");
});

test("a right axis carries the series it matches; without a match the chart keeps one axis", () => {
  const usage = { labels: { metric: "usage_pct", instance: "i-1" }, points: [[1, 40]] as Array<[number, number]> };
  const hitRate = { labels: { metric: "hit_rate_pct", instance: "i-1" }, points: [[1, 75]] as Array<[number, number]> };
  const rightAxis = { matches: (labels: Record<string, string>) => labels.metric === "hit_rate_pct", name: "命中率 %", yRange: [0, 100] as [number, number] };
  const option = buildTimeSeriesOption([usage, hitRate], { valueFmt: String, leftName: "使用率 %", yRange: [0, 100], rightAxis });
  const axes = option.yAxis as Array<{ name?: string; position: string; min?: number; max?: number; splitLine: { show?: boolean } }>;
  assert.equal(axes.length, 2);
  assert.deepEqual(axes.map((a) => [a.position, a.name, a.min, a.max]), [["left", "使用率 %", 0, 100], ["right", "命中率 %", 0, 100]]);
  assert.equal(axes[1].splitLine.show, false);
  assert.deepEqual((option.series as Array<{ yAxisIndex?: number }>).map((s) => s.yAxisIndex), [0, 1]);
  assert.equal((option.grid as { right: number }).right, 58);

  // An engine that reports no hit rate leaves only usage: one axis, as before.
  const single = buildTimeSeriesOption([usage], { valueFmt: String, leftName: "使用率 %", yRange: [0, 100], rightAxis });
  assert.equal(Array.isArray(single.yAxis), false);
  assert.equal((single.yAxis as { name: string }).name, "使用率 %");
  assert.equal((single.series as Array<{ yAxisIndex?: number }>)[0].yAxisIndex, undefined);
  assert.equal((single.grid as { right: number }).right, 20);
});
