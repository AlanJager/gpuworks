import assert from "node:assert/strict";
import { test } from "node:test";
import { applyChartGroup, buildTimeSeriesOption, metricSeriesID } from "../src/components/TimeSeriesChart.tsx";
import type { MetricSeries } from "../src/api";

function series(values: Array<[number, number]>): MetricSeries[] {
  return [{ labels: { instance: "inst-1" }, points: values }];
}

test("buildTimeSeriesOption draws no markLine when markLines is empty or omitted", () => {
  const withoutOption = buildTimeSeriesOption(series([[1000, 1]]), {});
  const emptyOption = buildTimeSeriesOption(series([[1000, 1]]), { markLines: [] });
  for (const option of [withoutOption, emptyOption]) {
    const [firstSeries] = option.series as Array<{ markLine?: unknown }>;
    assert.equal(firstSeries.markLine, undefined);
  }
});

test("buildTimeSeriesOption's first series carries a markLine entry per event, later series carry none", () => {
  const twoSeries: MetricSeries[] = [...series([[1000, 1]]), { labels: { instance: "inst-2" }, points: [[1000, 2]] }];
  const option = buildTimeSeriesOption(twoSeries, {
    markLines: [
      { time: 1500, label: "instance_running", color: "#57d344" },
      { time: 1800, label: "instance_failed", color: "#f93940" },
    ],
  });
  const [first, second] = option.series as Array<{ markLine?: { data: Array<{ xAxis: number; name: string }> } }>;
  assert.equal(first.markLine?.data.length, 2);
  assert.deepEqual(
    first.markLine?.data.map((d) => [d.xAxis, d.name]),
    [[1500, "instance_running"], [1800, "instance_failed"]],
  );
  assert.equal(second.markLine, undefined);
});

test("buildTimeSeriesOption ids each series with metricSeriesID and names them via seriesName", () => {
  const data = series([[1000, 1]]);
  const option = buildTimeSeriesOption(data, { seriesName: () => "Custom Name" });
  const [entry] = option.series as Array<{ id?: string; name?: string }>;
  assert.equal(entry.id, metricSeriesID(data[0].labels, 0));
  assert.equal(entry.name, "Custom Name");
});

test("applyChartGroup sets the chart's group and is a no-op without a group id", () => {
  const chart = { group: "" };
  applyChartGroup(chart, "monitor-overview");
  assert.equal(chart.group, "monitor-overview");

  const untouched = { group: "" };
  applyChartGroup(untouched, undefined);
  assert.equal(untouched.group, "");

  // A null chart (not yet mounted) must not throw.
  assert.doesNotThrow(() => applyChartGroup(null, "g"));
});
