import assert from "node:assert/strict";
import test from "node:test";
import { buildChatBody, streamOptionsBody } from "../src/pages/playground/api.ts";
import { newStreamTracker, recordDelta, recordUsage, streamMetrics } from "../src/pages/playground/streamMetrics.ts";
import type { ParameterState, StreamOptionDefinition } from "../src/pages/playground/types.ts";

const options: StreamOptionDefinition[] = [
  { name: "include_usage", label: { en: "", zh: "" }, description: { en: "", zh: "" }, default: true },
  { name: "continuous_usage_stats", label: { en: "", zh: "" }, description: { en: "", zh: "" }, default: true, requires: "include_usage" },
];
const state = (streamOptions?: Record<string, boolean>): ParameterState => ({ enabled: {}, values: {}, systemPrompt: "", extraJSON: "", streamOptions });

test("stream options default on, honour requires and drop out when all are off", () => {
  assert.deepEqual(streamOptionsBody(state(), options), { include_usage: true, continuous_usage_stats: true });
  assert.deepEqual(streamOptionsBody(state({ include_usage: true, continuous_usage_stats: false }), options), { include_usage: true });
  // continuous_usage_stats has no effect without include_usage.
  assert.equal(streamOptionsBody(state({ include_usage: false, continuous_usage_stats: true }), options), undefined);
  const body = buildChatBody("m", [{ role: "user", content: "hi" }], state({ include_usage: false, continuous_usage_stats: false }), options);
  assert.equal("stream_options" in body, false);
  assert.equal(body.stream, true);
  assert.deepEqual(buildChatBody("m", [], state(), options).stream_options, { include_usage: true, continuous_usage_stats: true });
});

test("live metrics use continuous usage and exclude prefill from speed", () => {
  const tracker = newStreamTracker(1000);
  // Prefill: the first chunk 200 ms after sending, with the prompt counted.
  recordUsage(tracker, { prompt_tokens: 400, completion_tokens: 0 });
  recordDelta(tracker, 1200);
  recordUsage(tracker, { prompt_tokens: 400, completion_tokens: 1 });
  // Then 20 ms per token.
  for (let token = 2; token <= 11; token++) {
    recordDelta(tracker, 1200 + (token - 1) * 20);
    recordUsage(tracker, { prompt_tokens: 400, completion_tokens: token });
  }
  const live = streamMetrics(tracker, 1500, false);
  assert.equal(live.ttftMS, 200);
  assert.equal(live.tpotMS, 20);
  assert.equal(live.tps, 50);
  assert.equal(live.prefillTPS, 2000);
  assert.equal(live.outputTokens, 11);
  assert.equal(live.approximate, undefined);
  assert.equal(live.durationMS, 500);
});

test("without continuous usage tokens are estimated from chunks until the final usage", () => {
  const tracker = newStreamTracker(0);
  recordDelta(tracker, 100);
  recordDelta(tracker, 130);
  recordDelta(tracker, 160);
  const live = streamMetrics(tracker, 170, false);
  assert.equal(live.outputTokens, 3);
  assert.equal(live.approximate, true);
  assert.equal(live.tpotMS, 30);
  assert.equal(live.prefillTPS, undefined, "no prompt count yet");
  // The final usage-only chunk arrives after the last text: 6 tokens (two per
  // chunk, as with speculative decoding).
  recordUsage(tracker, { prompt_tokens: 50, completion_tokens: 6 });
  assert.equal(tracker.continuousUsage, true, "usage after text counts as live");
  const done = streamMetrics(tracker, 200, true);
  assert.equal(done.outputTokens, 6);
  assert.equal(done.approximate, undefined);
  assert.equal(done.tpotMS, 12);
  assert.equal(done.prefillTPS, 500);
});

test("a reply that ends without usage stays approximate, and one token has no speed", () => {
  const tracker = newStreamTracker(0);
  recordDelta(tracker, 80);
  const done = streamMetrics(tracker, 90, true);
  assert.equal(done.ttftMS, 80);
  assert.equal(done.outputTokens, 1);
  assert.equal(done.approximate, true);
  assert.equal(done.tpotMS, undefined);
  assert.equal(done.tps, undefined);
  const empty = streamMetrics(newStreamTracker(0), 50, true);
  assert.deepEqual(empty, { durationMS: 50, ttftMS: undefined, tpotMS: undefined, tps: undefined, prefillTPS: undefined, outputTokens: undefined, approximate: undefined });
});
