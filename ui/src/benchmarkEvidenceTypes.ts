// Versioned benchmark evidence, matching pkg/bench/metrics.PointEvidence.
export interface BenchmarkMetricSummary {
  count: number;
  mean_ms: number;
  p50_ms: number;
  p99_ms: number;
}
export interface BenchmarkTokenDistribution {
  count: number;
  total: number;
  min: number;
  max: number;
  p50: number;
  p99: number;
}
export interface BenchmarkRequestEvidence {
  attempted: number;
  success: number;
  failed: number;
  canceled: number;
  ttft: BenchmarkMetricSummary;
  /** Absent from evidence recorded before first visible content was summarized. */
  first_content?: BenchmarkMetricSummary;
  no_content?: number;
  tpot: BenchmarkMetricSummary;
  last_output_latency: BenchmarkMetricSummary;
  completion: BenchmarkMetricSummary;
  tokens: {
    input: BenchmarkTokenDistribution;
    output: BenchmarkTokenDistribution;
    context: BenchmarkTokenDistribution;
    server_input: BenchmarkTokenDistribution;
    server_output: BenchmarkTokenDistribution;
    local_input: BenchmarkTokenDistribution;
    local_output: BenchmarkTokenDistribution;
    reasoning: BenchmarkTokenDistribution;
    failed_output: BenchmarkTokenDistribution;
    input_sources: Record<string, number> | null;
    output_sources: Record<string, number> | null;
  };
  cache: {
    reported: number;
    unknown: number;
    ratio_requests: number;
    successful_reported: number;
    input_tokens: number;
    cached_tokens: number;
  };
  finish_reasons: Record<string, number> | null;
  error_stages: Record<string, number> | null;
}
export interface BenchmarkSessionEvidence {
  session_id: string;
  planned_turns: number;
  attempted: number;
  success: number;
  failed: number;
  canceled: number;
  skipped: number;
  state: string;
  started_at: string | null;
  finished_at: string | null;
}
export interface BenchmarkPointEvidence {
  version: number;
  plan: {
    planned_sessions: number;
    planned_requests: number;
    turns_per_session?: number;
    output_budget: number;
    output_policy: string;
  };
  outcomes: {
    skipped: number;
    canceled_before_start: number;
    completed_sessions: number;
    failed_sessions: number;
    canceled_sessions: number;
    retry_count: number;
  };
  all: BenchmarkRequestEvidence;
  first_turn: BenchmarkRequestEvidence;
  followup_turns: BenchmarkRequestEvidence;
  by_turn: Record<string, BenchmarkRequestEvidence> | null;
  timing: {
    started_at: string | null;
    finished_at: string | null;
    completed_session_latency: BenchmarkMetricSummary;
  };
  execution: {
    seed: string | number;
    seed_derivation: string;
    cache_policy: string;
    cache_state: string;
    retry_policy: string;
    tokenizer_source: string;
    local_token_count_method: string;
    detail_policy: string;
    warmup_requests: number;
    warmup_succeeded: number;
  };
  sessions: BenchmarkSessionEvidence[] | null;
  sessions_truncated: boolean;
}
