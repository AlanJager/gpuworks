import assert from "node:assert/strict";
import test from "node:test";
import { DEFAULT_VISIBLE_POINT_COLUMNS, POINT_COLUMN_KEYS, defaultPointColumnPreference, parsePointColumnPreference, pointDerivedValues } from "../src/pages/evaluations/pointColumnPrefs.ts";
import type { BenchmarkPoint } from "../src/api.ts";

test("point column defaults keep required visible fields and hide optional detail fields", () => {
	assert.ok(DEFAULT_VISIBLE_POINT_COLUMNS.includes("point"));
	for (const key of ["interactivity", "latency_p99", "request_rate"] as const) assert.ok(DEFAULT_VISIBLE_POINT_COLUMNS.includes(key));
	for (const key of ["max_output", "max_total", "avg_ttft", "avg_tpot", "avg_latency", "success_rate", "ttft_p50", "tpot_p50", "latency_p50", "duration", "request_total"] as const) assert.ok(!DEFAULT_VISIBLE_POINT_COLUMNS.includes(key));
});

test("column preferences retain full drag order and restore defaults when corrupt", () => {
	const pref = defaultPointColumnPreference();
	pref.order = ["point", ...pref.order.slice(2), pref.order[1]];
	pref.visible = ["point", "sla"];
	assert.deepEqual(parsePointColumnPreference(JSON.stringify(pref)), pref);
	assert.deepEqual(parsePointColumnPreference("broken"), defaultPointColumnPreference());
	assert.deepEqual(parsePointColumnPreference(JSON.stringify({ order: POINT_COLUMN_KEYS, visible: ["concurrency"] })), defaultPointColumnPreference());
	assert.deepEqual(parsePointColumnPreference(JSON.stringify({ order: [...POINT_COLUMN_KEYS, "point"], visible: ["point"] })), defaultPointColumnPreference());
	// Unknown columns are dropped instead of discarding the whole layout.
	assert.deepEqual(parsePointColumnPreference(JSON.stringify({ order: [...POINT_COLUMN_KEYS, "unknown"], visible: ["point", "unknown"] })), { order: [...POINT_COLUMN_KEYS], visible: ["point"] });
});

test("a layout saved before interactivity keeps the user's choices and gains the new columns", () => {
	// The previous version's columns, reordered and with the per-user columns
	// visible.
	const saved = {
		order: ["point", "per_user_output", "concurrency", "length", "avg_output", "avg_total", "request", "per_user_total", "ttft_p99", "tpot_p99", "sla", "mark", "detail", "max_output", "max_total", "avg_ttft", "avg_tpot", "success_rate", "ttft_p50", "tpot_p50", "duration", "request_total"],
		visible: ["point", "per_user_output", "per_user_total", "concurrency", "max_output"],
	};
	const pref = parsePointColumnPreference(JSON.stringify(saved));
	assert.deepEqual(pref.order, ["point", "interactivity", "concurrency", "request_rate", "length", "avg_output", "avg_total", "request", "ttft_p99", "tpot_p99", "latency_p99", "sla", "mark", "detail", "max_output", "max_total", "avg_ttft", "avg_tpot", "avg_latency", "success_rate", "ttft_p50", "tpot_p50", "latency_p50", "duration", "request_total"]);
	// Per-user output became interactivity; per-user total is gone; columns
	// the layout never knew take their default visibility.
	assert.deepEqual(pref.visible, ["point", "interactivity", "concurrency", "max_output", "request_rate", "latency_p99"]);
});

test("derived point columns guard missing historical values", () => {
	const point = { concurrency: 2, output_tput: 20, total_tput: 30, p50_tpot: 8, success: 3, errors: 1, started_at: "2026-08-25T00:00:00Z", ended_at: "2026-08-25T00:00:12Z", max_output_tput: 0, max_total_tput: 0 } as BenchmarkPoint;
	assert.deepEqual(pointDerivedValues(point), { interactivity: 125, successRate: 75, requestTotal: 4, durationSeconds: 12, maxOutput: undefined, maxTotal: undefined });
	assert.equal(pointDerivedValues({ ...point, p50_tpot: 0 }).interactivity, undefined);
});
