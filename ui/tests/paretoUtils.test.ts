import assert from "node:assert/strict";
import test from "node:test";
import { paretoData, paretoTooltip } from "../src/pages/evaluations/paretoUtils.ts";
import type { BenchmarkPoint } from "../src/api.ts";

test("Pareto plots interactivity (1000 / TPOT P50) against output throughput with a complete tooltip", () => {
	// Throughput ÷ concurrency would put this point at 20 tok/s/user; its
	// decode-phase speed is 1000 / 12.5 ms = 80.
	const point = { seq: 2, concurrency: 4, target_isl: 64, target_osl: 16, actual_isl: 65.5, actual_osl: 15, output_tput: 80, total_tput: 100, request_tput: 3, p99_ttft: 20, p50_tpot: 12.5, p99_tpot: 15, p99_latency: 300, success: 9, errors: 1 } as BenchmarkPoint;
	const datum = paretoData([point])[0];
	assert.equal(datum.x, 80);
	assert.equal(datum.y, 80);
	const tooltip = paretoTooltip(datum);
	for (const text of ["Point #2", "C=4", "ISL=65.5", "OSL=15", "Interactivity: 80 tok/s/user", "Average total throughput", "Request throughput", "TTFT P99", "TPOT P50: 12.5 ms", "E2E latency P99: 300 ms", "Success rate: 90%"]) assert.match(tooltip, new RegExp(text));
});

test("points without a measured TPOT are left off the Pareto chart", () => {
	const measured = { seq: 1, concurrency: 1, output_tput: 10, p50_tpot: 20 } as BenchmarkPoint;
	const unmeasured = { seq: 2, concurrency: 2, output_tput: 15, p50_tpot: 0 } as BenchmarkPoint;
	assert.deepEqual(paretoData([measured, unmeasured]).map((datum) => datum.seq), [1]);
});

test("open-loop points are labeled by arrival rate", () => {
	const point = { seq: 3, concurrency: 0, request_rate: 2, target_isl: 8, target_osl: 4, output_tput: 50, p50_tpot: 10, success: 1, errors: 0 } as BenchmarkPoint;
	assert.match(paretoTooltip(paretoData([point])[0]), /R=2 req\/s · ISL=8 · OSL=4/);
});

test("Pareto omits unavailable v1 aggregate rates and timing while preserving measured zero", () => {
	const metric = { count: 2, mean_ms: 10, p50_ms: 10, p99_ms: 10 };
	const tokens = (count: number) => ({ count, total: 0, min: 0, max: 0, p50: 0, p99: 0 });
	const point = (seq: number, inputCount: number, tpotCount = 2) => ({
		seq, state: "failed", concurrency: 1, success: 2, errors: 0, output_tput: 0, total_tput: 0, request_tput: 2, p50_tpot: 10,
		long_tail: { evidence: { version: 1, all: { success: 2, ttft: metric, tpot: { ...metric, count: tpotCount }, last_output_latency: metric,
			tokens: { input: tokens(inputCount), output: tokens(2) } } } },
	} as BenchmarkPoint);
	const plotted = paretoData([point(1, 0), point(2, 2, 0), point(3, 2)]);
	assert.deepEqual(plotted.map(({ seq, x, y }) => ({ seq, x, y })), [{ seq: 3, x: 100, y: 0 }]);
	assert.match(paretoTooltip(plotted[0]), /Request throughput: 2 req\/s/);
});
