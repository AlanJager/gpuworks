import assert from "node:assert/strict";
import test from "node:test";
import type { BenchmarkMeta } from "../src/api.ts";
import { summarizeAgenticLoad } from "../src/pages/evaluations/agenticWorkload.ts";

const meta = {
	agentic_defaults: {
		preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753,
		output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8],
		sessions_per_point: { "1": 4, "2": 8, "4": 8, "8": 16 },
		sessions_per_concurrency: 2, min_sessions_per_point: 4,
	},
} as BenchmarkMeta;

test("agentic 80K derives the persisted workload shape", () => {
	const summary = summarizeAgenticLoad(79_998, 13, meta);
	assert.deepEqual({
		mean: summary.meanInputTokens, first: summary.firstPromptTokens, peak: summary.peakPromptTokens,
		context: summary.requiredContextTokens, sessions: summary.sessions, requests: summary.requests,
	}, { mean: 79_998, first: 74_160, peak: 85_836, context: 90_000, sessions: 36, requests: 468 });
	assert.ok(Math.abs(summary.designPrefixReuseRate - .9200018462) < 1e-9);
});

test("changing turns keeps mean input anchored", () => {
	const summary = summarizeAgenticLoad(79_998, 9, meta);
	assert.equal(summary.meanInputTokens, 79_998);
	assert.equal(summary.requests, 324);
	assert.ok(summary.firstPromptTokens < summary.meanInputTokens);
	assert.ok(summary.peakPromptTokens > summary.meanInputTokens);
});

test("custom session-concurrency points update run volume", () => {
	const summary = summarizeAgenticLoad(79_998, 13, meta, [2, 16]);
	assert.equal(summary.sessions, 40);
	assert.equal(summary.requests, 520);
});
