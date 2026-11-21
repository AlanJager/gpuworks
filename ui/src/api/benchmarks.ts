import type { BenchmarkPointEvidence } from "../benchmarkEvidenceTypes";
import type { ProductMessage } from "../apiError";
import type { LocalizedText } from "../i18n/localizedText";

export interface BenchmarkPoint {
  id: number;
  seq: number;
  // Closed loop: the concurrency held. Open loop (request_rate > 0): the
  // in-flight cap, 0 meaning unlimited.
  concurrency: number;
  target_isl: number;
  target_osl: number;
  // Open-loop arrival rate in req/s; 0 for a closed-loop point.
  request_rate?: number;
  state: string;
  success: number;
  errors: number;
  actual_isl: number;
  actual_osl: number;
  input_tokens: number;
  output_tokens: number;
  output_tput: number;
  total_tput: number;
  max_output_tput: number;
  max_total_tput: number;
  request_tput: number;
  avg_ttft: number;
  p50_ttft: number;
  p99_ttft: number;
  avg_tpot: number;
  p50_tpot: number;
  p99_tpot: number;
  avg_latency: number;
  p50_latency: number;
  p99_latency: number;
  long_tail?: BenchmarkLongTail;
  detail_path?: string;
  started_at?: string;
  ended_at?: string;
}
// BenchmarkLongTail is a point's long-tail facts: each percentile of TTFT,
// TPOT, end-to-end latency and inter-token latency in milliseconds, keyed by
// percentile ("50", "99"), and the cache and speculative-decoding facts.
export interface BenchmarkLongTail {
  ttft_ms: Record<string, number> | null;
  tpot_ms: Record<string, number> | null;
  latency_ms: Record<string, number> | null;
  itl_ms: Record<string, number> | null;
  approximate_tokens: boolean;
  kv_hit_rate?: number;
  cache_report_coverage?: number;
  peak_input_tokens: number;
  speculative?: { tokens_per_iteration: number; acceptance_rate: number };
  evidence?: BenchmarkPointEvidence;
}
export type BenchmarkPointSummary = Pick<
  BenchmarkPoint,
  | "seq"
  | "concurrency"
  | "target_isl"
  | "target_osl"
  | "request_rate"
  | "output_tput"
  | "request_tput"
  | "p99_ttft"
>;
export interface BenchmarkRun {
  id: number;
  name: string;
  target_kind: "instance" | "external";
  deployment_id?: number;
  instance_uuid?: string;
  base_url?: string;
  model: string;
  load_origin: string;
  /** The request priority class the run asked the gateway for; absent when it asked for none. */
  priority_class?: string;
  num_gpus: number;
  dataset: {
    kind?: string;
    preset?: string;
    generator_version?: number;
    input_tokens?: number;
    mean_input_tokens?: number;
    first_turn_input_tokens?: number;
    subsequent_turn_input_tokens?: number;
    output_tokens?: number;
    turns?: number;
    peak_input_tokens?: number;
    required_context_tokens?: number;
    design_prefix_reuse_rate?: number;
    seed?: number;
    workload_fingerprint?: string;
    ignore_eos?: boolean;
    profile?: BenchmarkDatasetProfile;
    samples?: Array<{
      messages: Array<{ role: string; content: string }>;
      follow_ups?: Array<{ role: string; content: string }>;
      input_tokens?: number;
      output_tokens?: number;
      wait_millis?: number;
    }>;
  };
  mode: "grid" | "saturation" | "sla";
  tokenizer_ref?: string;
  target_snapshot?: BenchmarkTargetSnapshot;
  warnings?: string[];
  deployment_deleted?: boolean;
  instance_deleted?: boolean;
  search?: {
    concurrency?: number | number[];
    isl?: number[];
    osl?: number[];
    max_concurrency?: number;
    fixed_concurrency?: number[];
    sessions_per_point?: Record<string, number>;
    min_throughput_gain?: number;
    requests_per_point_multiplier?: number;
    max_avg_ttft?: number;
    max_p99_ttft?: number;
    max_avg_tpot?: number;
    max_p99_tpot?: number;
    max_avg_latency?: number;
    max_p99_latency?: number;
    max_rps?: number;
    max_tps?: number;
    min_rps?: number;
    min_tps?: number;
    min_interactivity?: number;
    min_session_completion?: number;
    // Retired per-user limits (throughput ÷ concurrency); only historical
    // runs carry them.
    max_per_user_total_tps?: number;
    min_per_user_total_tps?: number;
    max_per_user_output_tps?: number;
    min_per_user_output_tps?: number;
    objective?: "output_tps" | "total_tps" | "rps";
    request_rates?: number[];
    burstiness?: number;
    max_in_flight?: number;
    point_duration_s?: number;
    warmup_requests?: number;
    request_timeout_s?: number;
  };
  keep_detail: boolean;
  detail_cap_mb: number;
  state: "pending" | "running" | "done" | "failed" | "canceled";
  verdict?: BenchmarkVerdict;
  progress?: {
    point_index?: number;
    point_total?: number;
    point_count?: number;
    completed?: number;
    errors?: number;
    current_point?: {
      seq: number;
      concurrency: number;
      target_isl: number;
      target_osl: number;
      request_total: number;
      request_rate?: number;
      warmup?: boolean;
    };
    sample?: {
      output_tput: number;
      total_tput: number;
      request_tput: number;
      ttft_p50: number;
      tpot_p50: number;
      latency_p99: number;
      success: number;
      errors: number;
    };
  };
  error?: string;
  /** The failed run's code; the console renders it from the code (benchmarkError.<code>). */
  failure?: ProductMessage;
  best_point?: BenchmarkPointSummary;
  points?: BenchmarkPoint[];
  created_at: string;
  started_at?: string;
  ended_at?: string;
}
export interface BenchmarkSnapshotGPU {
  index: number;
  name?: string;
  arch_id?: string;
  vendor?: string;
  uuid?: string;
  node_id?: number;
  node_name?: string;
}
export interface BenchmarkTargetSnapshot {
  target_kind: "instance" | "external";
  deployment_id?: number;
  deployment_name?: string;
  instance_uuid?: string;
  served_name?: string;
  model: string;
  model_source_id?: string;
  base_url?: string;
  load_origin: string;
  engine?: string;
  image?: string;
  command_mode?: string;
  entrypoint?: string[];
  argv?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  gpu_indexes?: number[];
  gpus?: BenchmarkSnapshotGPU[];
  num_gpus: number;
  tensor_parallel?: number;
  node_id?: number;
  node_name?: string;
}
export interface BenchmarkCurveSample {
  t: number;
  output_tput?: number | null;
  total_tput?: number | null;
  request_tput?: number | null;
  ttft_p50?: number | null;
  tpot_p50?: number | null;
  latency_p99?: number | null;
}
export interface BenchmarkCurveResponse {
  points: Array<{
    seq: number;
    concurrency: number;
    samples: BenchmarkCurveSample[];
  }>;
}
export interface BenchmarkTimelineRecord {
  version: number;
  run_id: number;
  point_seq: number;
  session_id: string;
  turn: number;
  planned_turns: number;
  attempt: number;
  finish_reason?: string;
  error_text?: string;
  canceled?: boolean;
  error_stage?: string;
  http_status?: number;
  input_source: string;
  output_source: string;
  output_budget: number;
  metrics: {
    started_at: string | null;
    first_output_at: string | null;
    first_content_at: string | null;
    last_output_at: string | null;
    finished_at: string | null;
    ttft_ms: number | null;
    tpot_ms: number | null;
    latency_ms: number | null;
    completion_ms: number | null;
    input_tokens: number;
    output_tokens: number;
  };
}
export interface BenchmarkRequestTimeline {
  status: "complete" | "disabled" | "unavailable" | "truncated";
  records: BenchmarkTimelineRecord[];
}
export interface BenchmarkViolation {
  reason?: "limit_exceeded" | "evidence_unavailable";
  metric: string;
  limit: number;
  actual: number;
}
export interface BenchmarkVerdict {
  kind:
    "saturated" | "truncated" | "sla_met" | "sla_none" | "grid_done" | "failed";
  best_point_seq?: number;
  saturation_concurrency?: number;
  headroom_pct?: number;
  sla_max_concurrency?: number;
  violated?: BenchmarkViolation[];
}
export interface BenchmarkMetaOption {
  value: string;
  label: LocalizedText;
  description?: LocalizedText;
  color?: string;
}
export interface BenchmarkModeDefaults {
  concurrency: number | number[];
  max_concurrency?: number;
  isl: number[];
  osl: number[];
  requests_per_point_multiplier: number;
  min_throughput_gain?: number;
  objective?: "output_tps" | "total_tps" | "rps";
}
export interface BenchmarkMeta {
  modes: BenchmarkMetaOption[];
  mode_defaults: Record<BenchmarkRun["mode"], BenchmarkModeDefaults>;
  datasets: BenchmarkMetaOption[];
  retired_datasets?: BenchmarkMetaOption[];
  agentic_presets: Array<{
    value: string;
    label: string;
    mean_input_tokens: number;
    first_turn_input_tokens: number;
    peak_input_tokens: number;
    required_context_tokens: number;
    recommended?: boolean;
  }>;
  agentic_defaults: {
    preset: string;
    turns: number;
    subsequent_turn_input_tokens: number;
    output_tokens: number;
    seed: number;
    concurrency: number[];
    sessions_per_point: Record<string, number>;
    sessions_per_concurrency: number;
    min_sessions_per_point: number;
  };
  dataset_profiles: Record<string, BenchmarkDatasetProfile>;
  sla_constraints: Array<{
    key: string;
    label: LocalizedText;
    unit?: string;
    direction: "min" | "max";
    min: number;
    max: number;
    step: number;
  }>;
  objectives: BenchmarkMetaOption[];
  statuses: BenchmarkMetaOption[];
  tokenizer_schemes: Record<string, string>;
  detail_cap_mb: { default: number; min: number; max: number; unit: string };
  load_defaults: BenchmarkLoadDefaults;
}
export interface BenchmarkLoadDefaults {
  warmup_requests: number;
  max_warmup_requests: number;
  burstiness: number;
  point_duration_s: number;
  min_point_duration_s: number;
  max_point_duration_s: number;
  min_open_loop_requests: number;
  max_request_rate: number;
  max_request_rates: number;
  max_burstiness: number;
  max_in_flight: number;
  request_timeout_s: number;
  min_request_timeout_s: number;
  max_request_timeout_s: number;
  ignore_eos: boolean;
  ignore_eos_datasets: string[];
}
export interface BenchmarkDatasetProfile {
  sample_count: number;
  max_input_tokens: number;
  max_output_tokens: number;
}
