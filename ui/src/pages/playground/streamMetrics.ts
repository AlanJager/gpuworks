import type { PerformanceMetrics, Usage } from "./types";

// StreamTracker collects the timing facts of one streamed reply as it
// arrives. Times are performance.now() milliseconds.
export interface StreamTracker {
  startedAt: number;
  firstDeltaAt?: number;
  lastDeltaAt?: number;
  // Chunks that carried content or reasoning text.
  chunks: number;
  usage?: Usage;
  // Whether usage arrived while text was still streaming, i.e. the endpoint
  // honours stream_options.continuous_usage_stats.
  continuousUsage: boolean;
}

export function newStreamTracker(startedAt: number): StreamTracker {
  return { startedAt, chunks: 0, continuousUsage: false };
}

// recordDelta notes one chunk that carried generated text.
export function recordDelta(tracker: StreamTracker, at: number) {
  tracker.firstDeltaAt ??= at;
  tracker.lastDeltaAt = at;
  tracker.chunks += 1;
}

export function recordUsage(tracker: StreamTracker, usage: Usage) {
  tracker.usage = usage;
  if ((usage.completion_tokens ?? 0) > 0 && tracker.lastDeltaAt !== undefined) tracker.continuousUsage = true;
}

// streamMetrics derives the reply's metrics at time now. Speed is measured
// over the decode phase only, from the first to the last text chunk, so it
// never includes prefill:
//
//   TTFT = first chunk − request sent
//   TPOT = decode time ÷ (tokens − 1),  TPS = 1000 / TPOT (tok/s per request)
//   Prefill = prompt tokens ÷ TTFT
//
// Token counts come from usage when the endpoint reports them (on every chunk
// with continuous_usage_stats, or once at the end); otherwise each text chunk
// counts as one token and the result is marked approximate.
export function streamMetrics(tracker: StreamTracker, now: number, done: boolean): PerformanceMetrics {
  const exact = tracker.usage?.completion_tokens && (done || tracker.continuousUsage) ? tracker.usage.completion_tokens : undefined;
  const tokens = exact ?? tracker.chunks;
  const ttftMS = tracker.firstDeltaAt === undefined ? undefined : tracker.firstDeltaAt - tracker.startedAt;
  const decodeMS = tracker.firstDeltaAt === undefined || tracker.lastDeltaAt === undefined ? undefined : tracker.lastDeltaAt - tracker.firstDeltaAt;
  const tpotMS = decodeMS !== undefined && decodeMS > 0 && tokens > 1 ? decodeMS / (tokens - 1) : undefined;
  const prompt = tracker.usage?.prompt_tokens;
  return {
    durationMS: now - tracker.startedAt,
    ttftMS,
    tpotMS,
    tps: tpotMS ? 1000 / tpotMS : undefined,
    prefillTPS: ttftMS && prompt ? prompt / (ttftMS / 1000) : undefined,
    outputTokens: tokens || undefined,
    approximate: exact === undefined && tokens > 0 ? true : undefined,
  };
}
