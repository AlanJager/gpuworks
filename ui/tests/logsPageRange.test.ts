import assert from "node:assert/strict";
import test from "node:test";

import { RANGE_MS, histogramBucketMS, initialAbsoluteWindow } from "../src/pages/logsPageRange.ts";

function params(query: string): URLSearchParams {
  return new URLSearchParams(query);
}

test("initialAbsoluteWindow accepts a valid from/to pair", () => {
  assert.deepEqual(initialAbsoluteWindow(params("from=1000&to=5000")), { from: 1000, to: 5000 });
});

test("initialAbsoluteWindow rejects a window missing either bound", () => {
  assert.equal(initialAbsoluteWindow(params("from=1000")), null);
  assert.equal(initialAbsoluteWindow(params("to=5000")), null);
  assert.equal(initialAbsoluteWindow(params("")), null);
});

test("initialAbsoluteWindow rejects non-numeric bounds", () => {
  assert.equal(initialAbsoluteWindow(params("from=abc&to=def")), null);
  assert.equal(initialAbsoluteWindow(params("from=1000&to=nope")), null);
});

test("initialAbsoluteWindow rejects to <= from", () => {
  assert.equal(initialAbsoluteWindow(params("from=5000&to=1000")), null, "to before from");
  assert.equal(initialAbsoluteWindow(params("from=1000&to=1000")), null, "to equal to from (an empty window)");
});

test("initialAbsoluteWindow rejects a non-positive from", () => {
  assert.equal(initialAbsoluteWindow(params("from=0&to=5000")), null);
  assert.equal(initialAbsoluteWindow(params("from=-100&to=5000")), null);
});

test("histogramBucketMS passes presets straight through the fixed table", () => {
  assert.equal(histogramBucketMS("30m", null), 60_000);
  assert.equal(histogramBucketMS("1h", null), 120_000);
  assert.equal(histogramBucketMS("6h", null), 720_000);
  assert.equal(histogramBucketMS("24h", null), 1_800_000);
  assert.equal(histogramBucketMS("7d", null), 10_800_000);
  assert.equal(histogramBucketMS("all", null), 10_800_000);
});

test("histogramBucketMS falls back to the 24h bucket for a custom range with no window yet", () => {
  assert.equal(histogramBucketMS("custom", null), 1_800_000);
});

test("histogramBucketMS picks the smallest preset bucket whose own window still covers a custom span", () => {
  const to = 100_000_000_000;
  const casesMS: Array<[number, number]> = [
    [RANGE_MS["30m"], 60_000],
    [RANGE_MS["1h"], 120_000],
    [RANGE_MS["6h"], 720_000],
    [RANGE_MS["24h"], 1_800_000],
    [RANGE_MS["7d"] + 1, 10_800_000],
  ];
  for (const [span, expected] of casesMS) {
    assert.equal(histogramBucketMS("custom", { from: to - span, to }), expected, `span=${span}`);
  }
});
