import assert from "node:assert/strict";
import { test } from "node:test";
import { buildTimeSeriesOption, ZERO_SPAN_PAD_MS } from "../src/components/TimeSeriesChart.tsx";
import type { MetricSeries } from "../src/api";
import { rangeWindow } from "../src/monitorRange.ts";

const MIN = 60_000;
const DAY = 86_400_000;
const T0 = new Date(2026, 10, 20, 12, 0).getTime();

type Axis = { min?: number; max?: number; axisLabel: { formatter?: (value: number) => string } };
type Line = { showSymbol: boolean; symbolSize?: number; data: Array<[number, number]> };
const xAxis = (option: ReturnType<typeof buildTimeSeriesOption>) => option.xAxis as Axis;
const lines = (option: ReturnType<typeof buildTimeSeriesOption>) => option.series as unknown as Line[];
const one = (points: Array<[number, number]>, labels: Record<string, string> = { instance: "i-1" }): MetricSeries[] => [{ labels, points }];
const ramp = (n: number, step = 10_000): Array<[number, number]> => Array.from({ length: n }, (_, i) => [T0 + i * step, i]);

test("xRange pins the time axis to the query window, whatever the data covers", () => {
  const window: [number, number] = [T0 - 15 * MIN, T0];
  // One scrape in the last interval: the axis is the 15 minutes asked for.
  const axis = xAxis(buildTimeSeriesOption(one([[T0 - 10_000, 0.753]]), { xRange: window }));
  assert.deepEqual([axis.min, axis.max], window);
  assert.equal(axis.max! - axis.min!, 15 * MIN, "not ECharts' day either side of a single point");
  // Data filling the window changes nothing.
  const full = xAxis(buildTimeSeriesOption(one([[T0 - 15 * MIN, 1], [T0, 2]]), { xRange: window }));
  assert.deepEqual([full.min, full.max], window);
  // No series at all (the empty state sits on top): the axis still holds the window.
  const empty = xAxis(buildTimeSeriesOption([], { xRange: window }));
  assert.deepEqual([empty.min, empty.max], window);
});

test("xRange widens to take in points outside it and never clips them", () => {
  const window: [number, number] = [T0 - 15 * MIN, T0];
  // Step rounding or a clock offset puts a point past either end.
  const late = xAxis(buildTimeSeriesOption(one([[T0 - 5 * MIN, 1], [T0 + 7_000, 2]]), { xRange: window }));
  assert.deepEqual([late.min, late.max], [window[0], T0 + 7_000]);
  const early = xAxis(buildTimeSeriesOption(one([[window[0] - 30_000, 1], [T0 - MIN, 2]]), { xRange: window }));
  assert.deepEqual([early.min, early.max], [window[0] - 30_000, T0]);
  // Every series counts, not just the first.
  const second = xAxis(buildTimeSeriesOption([...one([[T0 - MIN, 1]]), ...one([[T0 + 3 * MIN, 1]], { instance: "i-2" })], { xRange: window }));
  assert.deepEqual([second.min, second.max], [window[0], T0 + 3 * MIN]);
  // The data itself is passed through untouched.
  const option = buildTimeSeriesOption(one([[T0 + 7_000, 2]]), { xRange: window });
  assert.deepEqual(lines(option)[0].data, [[T0 + 7_000, 2]]);
});

test("a window that cannot bound an axis is ignored", () => {
  const points = ramp(5);
  for (const bad of [[T0, T0], [T0, T0 - MIN], [Number.NaN, T0], [T0, Infinity], [undefined as unknown as number, T0]] as Array<[number, number]>) {
    const axis = xAxis(buildTimeSeriesOption(one(points), { xRange: bad }));
    assert.deepEqual([axis.min, axis.max], [undefined, undefined], JSON.stringify(bad));
  }
});

test("a lone point shows its symbol; a drawn line does not", () => {
  const lone = lines(buildTimeSeriesOption(one([[T0, 0.753]]), {}))[0];
  assert.equal(lone.showSymbol, true);
  assert.equal(typeof lone.symbolSize, "number");
  const drawn = lines(buildTimeSeriesOption(one(ramp(3)), {}))[0];
  assert.equal(drawn.showSymbol, false);
  assert.equal("symbolSize" in drawn, false, "a multi-point line is configured as before");
  // Per line: the lone one shows, its neighbour does not.
  const mixed = lines(buildTimeSeriesOption([...one(ramp(4)), ...one([[T0, 1]], { instance: "i-2" })], {}));
  assert.deepEqual(mixed.map((line) => line.showSymbol), [false, true]);
});

test("gaps: with connectNulls an unavailable neighbour leaves no point alone, without it an isolated point shows", () => {
  const gap = Number.NaN;
  const withGap: Array<[number, number]> = [[T0, 1], [T0 + 10_000, gap], [T0 + 20_000, 3], [T0 + 30_000, 4]];
  // Kept gaps: the point at T0 sits between the start and a gap, so draws nothing.
  assert.equal(lines(buildTimeSeriesOption(one(withGap), { connectNulls: false }))[0].showSymbol, true);
  // Joined across the gap, the three finite points form a line.
  assert.equal(lines(buildTimeSeriesOption(one(withGap), { connectNulls: true }))[0].showSymbol, false);
  // Kept gaps but every finite point has a finite neighbour.
  const pairs: Array<[number, number]> = [[T0, 1], [T0 + 10_000, 2], [T0 + 20_000, gap], [T0 + 30_000, 3], [T0 + 40_000, 4]];
  assert.equal(lines(buildTimeSeriesOption(one(pairs), { connectNulls: false }))[0].showSymbol, false);
  // One finite point among unavailable ones is a lone point either way.
  const only: Array<[number, number]> = [[T0, gap], [T0 + 10_000, 5], [T0 + 20_000, gap]];
  assert.equal(lines(buildTimeSeriesOption(one(only), { connectNulls: true }))[0].showSymbol, true);
});

test("without a window, points that share one timestamp get a padded axis, not ECharts' day either side", () => {
  const axis = xAxis(buildTimeSeriesOption(one([[T0, 0.753]]), {}));
  assert.deepEqual([axis.min, axis.max], [T0 - ZERO_SPAN_PAD_MS, T0 + ZERO_SPAN_PAD_MS]);
  assert.equal(ZERO_SPAN_PAD_MS, 5 * MIN);
  assert.ok(axis.max! - axis.min! < DAY / 100);
  // Several lines on the same instant are still zero-span.
  const many = xAxis(buildTimeSeriesOption([...one([[T0, 1]]), ...one([[T0, 2]], { instance: "i-2" })], {}));
  assert.deepEqual([many.min, many.max], [T0 - ZERO_SPAN_PAD_MS, T0 + ZERO_SPAN_PAD_MS]);
  // A padded axis is spaced like any other: a formatter is installed.
  assert.equal(typeof axis.axisLabel.formatter, "function");
});

test("a chart with data over a span and no window keeps the axis the data gives it", () => {
  const axis = xAxis(buildTimeSeriesOption(one(ramp(6)), {}));
  assert.equal(axis.min, undefined);
  assert.equal(axis.max, undefined);
  assert.equal(xAxis(buildTimeSeriesOption([], {})).min, undefined);
});

test("label spacing follows the window span when one is given", () => {
  // Two points a minute apart in a 15 minute window: spaced by the window
  // (every 3 minutes), not by the data's minute (which would label every tick).
  const ticks = Array.from({ length: 16 }, (_, i) => T0 - 15 * MIN + i * MIN);
  const windowed = xAxis(buildTimeSeriesOption(one([[T0 - MIN, 1], [T0, 2]]), { xRange: [T0 - 15 * MIN, T0] })).axisLabel.formatter!;
  const shown = ticks.map(windowed).filter(Boolean);
  assert.ok(shown.length >= 5 && shown.length <= 6, `about five labels, got ${shown.length}: ${shown.join(" ")}`);
  assert.deepEqual(shown.slice(0, 2), ["11:45", "11:48"]);
  const dataOnly = xAxis(buildTimeSeriesOption(one([[T0 - MIN, 1], [T0, 2]]), {})).axisLabel.formatter!;
  assert.equal(ticks.map(dataOnly).filter(Boolean).length, 16, "without the window every minute tick is labelled");
  // A window over a day spells the date out, as a data span over a day does.
  const week = xAxis(buildTimeSeriesOption(one([[T0, 1]]), { xRange: [T0 - 7 * DAY, T0] })).axisLabel.formatter!;
  assert.match(week(T0 - 7 * DAY), /^\d\d-\d\d \d\d:\d\d$/);
  // The labels still land on the widened axis when data pokes out of the window.
  const widened = xAxis(buildTimeSeriesOption(one([[T0 + 6 * MIN, 1]]), { xRange: [T0 - 15 * MIN, T0] }));
  assert.equal(widened.max, T0 + 6 * MIN);
});

test("axis options for typical multi-point input are the ones the chart always had", () => {
  const input = [...one(ramp(90), { instance: "a" }), ...one(ramp(90), { instance: "b" })];
  const option = buildTimeSeriesOption(input, { unit: "tok/s", seriesName: (labels) => labels.instance });
  const { axisLabel, ...axis } = option.xAxis as Axis & Record<string, unknown>;
  assert.deepEqual(axis, {
    type: "time", boundaryGap: [0, 0],
    axisLine: { lineStyle: { color: (option.xAxis as { axisLine: { lineStyle: { color: string } } }).axisLine.lineStyle.color } },
    splitLine: { lineStyle: { color: (option.xAxis as { splitLine: { lineStyle: { color: string } } }).splitLine.lineStyle.color, type: "dashed" } },
  });
  assert.deepEqual(Object.keys(axisLabel).sort(), ["color", "formatter", "hideOverlap"]);
  for (const line of lines(option)) {
    assert.equal(line.showSymbol, false);
    assert.equal("symbolSize" in line, false);
  }
  const y = option.yAxis as { scale: boolean; min?: number; max?: number };
  assert.deepEqual([y.scale, y.min, y.max], [true, undefined, undefined]);
});

test("a ratio chart's pinned 0..100 axis fits a lone reading and labels every tick the same way", () => {
  const y = buildTimeSeriesOption(one([[T0, 75.3]]), { yRange: [0, 100], valueFmt: (v) => v.toFixed(1) }).yAxis as {
    min: number; max: number; interval: number; scale: boolean; axisLabel: { formatter: (value: number, index: number) => string };
  };
  assert.deepEqual([y.min, y.max, y.interval, y.scale], [0, 100, 20, false]);
  const ticks = [0, 20, 40, 60, 80, 100].map((tick, index) => y.axisLabel.formatter(tick, index));
  assert.deepEqual(ticks, ["0.0", "20.0", "40.0", "60.0", "80.0", "100.0"]);
});

test("rangeWindow is the window a monitor range covers", () => {
  const now = T0;
  assert.deepEqual(rangeWindow("15m", now), [now - 15 * MIN, now]);
  assert.deepEqual(rangeWindow("168h", now), [now - 7 * DAY, now]);
  assert.deepEqual(rangeWindow(`${T0 - 3 * DAY}~${T0 - 2 * DAY}`, now), [T0 - 3 * DAY, T0 - 2 * DAY]);
});
