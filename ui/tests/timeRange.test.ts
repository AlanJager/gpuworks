import assert from "node:assert/strict";
import test from "node:test";
import {
  QUICK_RANGES,
  RETENTION_MS,
  isBeyondRetention,
  quickRange,
  resolveRange,
  selectionLabelKey,
  stepString,
  type TimeRangeSelection,
} from "../src/timeRange.ts";

const NOW = 1_800_000_000_000;
const MIN = 60 * 1000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

test("every quick range carries label, step, and poll metadata", () => {
  const keys = QUICK_RANGES.map((range) => range.key);
  assert.deepEqual(keys, ["15m", "1h", "6h", "1d", "1w", "1mo", "1y"]);
  for (const range of QUICK_RANGES) {
    assert.ok(range.labelKey.startsWith("timeRange."), range.labelKey);
    assert.ok(range.stepMS > 0 && range.pollMs > 0, `${range.key} must define step+poll`);
  }
  assert.equal(quickRange("15m").ms, 15 * MIN);
});

test("quick ranges anchor the window at now so polls slide forward", () => {
  const resolved = resolveRange({ kind: "quick", key: "15m" }, NOW);
  assert.deepEqual(
    { startMS: resolved.startMS, endMS: resolved.endMS },
    { startMS: NOW - 15 * MIN, endMS: NOW },
  );
  assert.equal(resolved.stepMS, 10_000);
  assert.equal(resolved.pollMs, 10_000);
  assert.equal(resolved.beyondRetention, false);
});

test("polling cadence widens as the window grows", () => {
  const cadence: Array<[string, number]> = QUICK_RANGES.map((range) => [
    range.key,
    resolveRange({ kind: "quick", key: range.key }, NOW).pollMs,
  ]);
  for (let i = 1; i < cadence.length; i++) {
    assert.ok(
      cadence[i][1] >= cadence[i - 1][1],
      `${cadence[i][0]} (${cadence[i][1]}ms) must not poll faster than ${cadence[i - 1][0]}`,
    );
  }
  assert.equal(resolveRange({ kind: "quick", key: "1d" }, NOW).pollMs, 60_000);
  assert.equal(resolveRange({ kind: "quick", key: "1w" }, NOW).pollMs, 5 * MIN);
});

test("ranges beyond the 7-day retention are flagged, 1 week exactly is not", () => {
  assert.equal(isBeyondRetention({ kind: "quick", key: "1w" }, NOW), false);
  assert.equal(RETENTION_MS, 7 * DAY);
  assert.equal(isBeyondRetention({ kind: "quick", key: "1mo" }, NOW), true);
  assert.equal(isBeyondRetention({ kind: "quick", key: "1y" }, NOW), true);
});

test("custom ranges are one-shot and keep the caller's bounds", () => {
  const resolved = resolveRange(
    { kind: "custom", startMS: NOW - 2 * DAY, endMS: NOW - DAY },
    NOW,
  );
  assert.equal(resolved.startMS, NOW - 2 * DAY);
  assert.equal(resolved.endMS, NOW - DAY);
  assert.equal(resolved.pollMs, 0, "a historical window must not poll");
  assert.equal(resolved.beyondRetention, false);

  const stale = resolveRange({ kind: "custom", startMS: NOW - 30 * DAY, endMS: NOW - 20 * DAY }, NOW);
  assert.equal(stale.beyondRetention, true);
});

test("custom windows pick the step of the enclosing quick range", () => {
  assert.equal(resolveRange({ kind: "custom", startMS: NOW - 10 * MIN, endMS: NOW }, NOW).stepMS, 10_000);
  assert.equal(resolveRange({ kind: "custom", startMS: NOW - 2 * HOUR, endMS: NOW }, NOW).stepMS, 30_000);
  assert.equal(resolveRange({ kind: "custom", startMS: NOW - 2 * DAY, endMS: NOW }, NOW).stepMS, 10 * MIN);
});

test("stepString only emits units Go's ParseDuration understands", () => {
  assert.equal(stepString(10_000), "10s");
  assert.equal(stepString(30 * MIN), "30m");
  assert.equal(stepString(2 * HOUR), "2h");
  // "24h", never "1d": Go has no day unit and would silently fall back.
  assert.equal(stepString(DAY), "24h");
});

test("selection label keys separate quick, custom, and tail modes", () => {
  assert.equal(selectionLabelKey({ kind: "quick", key: "6h" }), "timeRange.6h");
  assert.equal(selectionLabelKey({ kind: "custom", startMS: 0, endMS: 1 }), "timeRange.custom");
  const quick: TimeRangeSelection = { kind: "quick", key: "1h" };
  assert.equal(quick.key, "1h");
});
