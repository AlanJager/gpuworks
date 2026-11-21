import type { MonitorRange } from "../monitorRange";
import { rangeParams } from "../monitorRange";
import type { PdRoleName } from "./engines";
import type { DeploymentHealth } from "./deployments";
import type { MetricSeries } from "./metrics";

// --- Monitor (/api/v1/monitor) ---------------------------------------------
// Shapes mirror the wire contract exactly (all snake_case; points are
// [t, v] pairs, the same MetricSeries shape GET /metrics/query answers).

// A signal reports a fleet fact and passes no verdict on it.
export interface MonitorSignal {
  key: string;
  collected: boolean;
  value?: number;
  denominator?: number;
  // error_rate carries {"5xx", "429", "asleep_503"} (each a percent);
  // ready_replicas carries {"not_ready": string[]}; ttft/tpot carry
  // {"upper_bound": true} - value/denominator (p95/p99) are each the max,
  // across the window, of the per-tick quantile gauge, not a single
  // quantile over the whole window (monitor.Window's doc comment) - so they
  // read as a conservative upper bound, not an exact percentile. Any
  // collected signal may additionally carry {"no_recent_requests": true}
  // when the window has samples but nothing happened in the last 5
  // minutes - value/denominator stay real numbers (0 for a rate) except
  // for ttft/tpot, where the server omits them (null) since a quantile has
  // no meaningful zero.
  detail?: Record<string, unknown>;
  spark?: Array<[number, number]>;
}

export type MonitorSeries = MetricSeries;

export interface MonitorReplicaCount {
  ready: number;
  desired: number;
}

/**
 * Where a deployment runs, as the server states it: node names in node-rank
 * order, the widest replica's node count, the most cards on one node, and the
 * tensor-parallel degree only when the platform knows it (absent for a
 * callable recipe or instances stating different degrees). The client formats
 * and localises it (pages/monitor/placement.ts) and derives nothing.
 */
export interface MonitorPlacement {
  nodes: string[];
  nnodes: number;
  gpus_per_node: number;
  tensor_parallel?: number;
}

export interface MonitorService {
  deployment_id: number;
  name: string;
  engine: string;
  placement: MonitorPlacement;
  replicas: MonitorReplicaCount;
  state: "running" | "starting" | "stopped" | "failed" | "asleep" | "offline";
  request_rate: number | null;
  ttft_p95_ms: number | null;
  running: number | null;
  waiting: number | null;
  kv_usage_pct: number | null;
  kv_hit_rate_pct: number | null;
  prompt_tok_s: number | null;
  generation_tok_s: number | null;
  // The deployment's health check.
  health: DeploymentHealth;
}

export interface MonitorVramNode {
  node_uuid: string;
  name: string;
  gpu_count: number;
  total_bytes: number;
  allocated_bytes: number;
  // Null when the node has no fresh nvidia-smi memory.used sample
  // (dto.MonitorVRAMNode.UsedBytes/UsedAsOf are *int64).
  used_bytes: number | null;
  used_as_of: number | null;
}

export interface MonitorEvent {
  time_ms: number;
  kind:
    | "instance_running"
    | "instance_failed"
    | "instance_stopped"
    | "sleep"
    | "wake"
    | "node_offline"
    | "node_online";
  // Always present on the wire (no omitempty) - "" / 0 for a node-level
  // event, which has no owning deployment or instance.
  deployment_id: number;
  instance_uuid: string;
  label: string;
}

export interface MonitorStateCount {
  state: MonitorService["state"];
  count: number;
}

export interface MonitorOverview {
  as_of: number;
  stale: boolean;
  signals: MonitorSignal[];
  services: MonitorService[];
  // services[] counted by state: every state, in the order the state
  // summary lists them, zero counts included (dto.MonitorStateCount).
  state_counts: MonitorStateCount[];
  vram_by_node: MonitorVramNode[];
  cluster_tokens: { prompt: MonitorSeries; generation: MonitorSeries };
  events: MonitorEvent[];
}

export interface MonitorDeploymentSummary {
  id: number;
  name: string;
  engine: string;
  placement: MonitorPlacement;
  replicas: MonitorReplicaCount;
}

export interface MonitorAlignment {
  proxy_requests: number;
  engine_completed: number;
  // client_errors is the client's own fault (4xx plus 429); server_errors is
  // the server's fault (5xx plus the asleep-instance 503) - see
  // dto.MonitorAlignment's doc comment. Both are disjoint from each other
  // and from engine_completed; lost is max(0, proxy_requests -
  // engine_completed - client_errors - server_errors) - in_flight is NOT
  // subtracted, so requests still streaming legitimately show up in lost.
  client_errors: number;
  server_errors: number;
  lost: number;
  // in_flight is the proxy's in-flight gauge at the end of the window.
  // discrepancy_persistent is true when lost exceeds the window's maximum
  // in-flight count - i.e. more requests are missing than could have been
  // in flight at any point, so the gap cannot be explained by streaming
  // requests. The UI shows the red "lost" sentence only when it is true.
  in_flight: number;
  discrepancy_persistent: boolean;
}

export interface MonitorTtftBreakdown {
  queue_ms: number;
  prefill_ms: number;
  first_token_ms: number;
  total_ms: number;
  // True when queue_ms/prefill_ms/first_token_ms needed adjusting (scaled
  // down, and/or first_token_ms clamped to 0) to actually sum to total_ms -
  // see dto.MonitorTTFTBreakdown's doc comment. The UI marks the caption
  // "approx." rather than presenting the bar as an exact decomposition.
  approximate: boolean;
}

export interface MonitorReplicaGPU {
  index: number;
  util_pct: number | null;
  mem_bw_pct: number | null;
  temperature_c: number | null;
  power_w: number | null;
}

export interface MonitorReplica {
  instance_uuid: string;
  state: string;
  node_name: string;
  gpu_indexes: number[];
  running: number | null;
  waiting: number | null;
  kv_usage_pct: number | null;
  kv_hit_rate_pct: number | null;
  prompt_tok_s: number | null;
  generation_tok_s: number | null;
  gpus: MonitorReplicaGPU[];
  // Set for a PD deployment's replica (ADR 0028); absent for an ordinary
  // deployment, which never renders a role badge.
  role?: PdRoleName | string;
}

// MonitorDeploymentPd is the service monitor page's PD summary (ADR 0028):
// present only when the deployment is a PD deployment. Every field is null,
// not omitted, when the window has no samples to compute it from - same
// null-means-not-collected contract as the rest of this file, so the UI
// renders a dash rather than a fabricated 0.
export interface MonitorDeploymentPd {
  effectiveness_pct: number | null;
  fallback_rate_pct: number | null;
  fallbacks: Record<string, number>;
  prefill_stage_avg_ms: number | null;
  prefill_waiting_avg: number | null;
  avg_prompt_tokens: number | null;
  // The prefill stage's p95 over the same window.
  prefill_stage_p95_ms: number | null;
  // The decode instances' KV transfer, per transfer. exposed is false when
  // the PD mode's engine exports no transfer metrics (vllm-ascend's
  // Mooncake connector); every value is then null.
  kv_transfer: {
    exposed: boolean;
    avg_ms: number | null;
    p95_ms: number | null;
    avg_mb: number | null;
    mb_per_s: number | null;
    failed: number | null;
    expired: number | null;
  };
}

export interface MonitorDeploymentCharts {
  latency: MonitorSeries[];
  tpot: MonitorSeries[];
  concurrency: MonitorSeries[];
  kv: MonitorSeries[];
  throughput: MonitorSeries[];
  stage_time: MonitorSeries[];
  preemptions: MonitorSeries[];
}

// GET /api/v1/monitor/deployments/{id}/signals: the series the deployment
// drawer's metrics panel draws over a window it picked, each signal from the
// engine or, when the engine has none, the gateway (ADR 0039). A series from
// the engine carries an `instance` label; one from the gateway does not, as
// the gateway measures the deployment, not its instances.
export interface MonitorDeploymentSignals {
  start_ms: number;
  end_ms: number;
  step_ms: number;
  // Only running, prompt_tokens and generation_tokens; absent without data.
  sources: Partial<Record<MonitorSignalSourceKey, MonitorSignalSource>>;
  // labels.metric: running | waiting
  concurrency: MonitorSeries[];
  // labels.metric: usage_pct (a percentage)
  kv: MonitorSeries[];
  // labels.metric: prompt_tok_s | generation_tok_s (rates)
  throughput: MonitorSeries[];
}

export type MonitorSignalSourceKey =
  | "ttft"
  | "e2e"
  | "prompt_tokens"
  | "generation_tokens"
  | "running"
  | "tpot"
  | "queue_time"
  | "prefill_time"
  | "decode_time"
  | "kv_hit_rate";
export type MonitorSignalSource = "engine" | "gateway";

// engineReportsNoMetrics: every signal with data came from the gateway, so
// the engine itself reports nothing (Strata has no /metrics, or a callable
// template declares no mapping).
export function engineReportsNoMetrics(sources: MonitorDeployment["sources"]): boolean {
  const values = Object.values(sources ?? {});
  return values.length > 0 && values.every((source) => source === "gateway");
}

// One stored series from the metric catalog GET /api/v1/monitor/metrics. An
// inference histogram contributes its quantile gauges (quantiles: true) and
// its _sum/_count.
export type MonitorMetricSource = "gpu" | "dgpu" | "node" | "network" | "engine" | "gateway" | "kv_disk" | "benchmark" | "peer_transfer";
export interface MonitorMetricSeriesInfo {
  series: string;
  name: string;
  source: MonitorMetricSource;
  kind: "counter" | "gauge" | "histogram";
  unit: string;
  quantiles: boolean;
  labels?: string[];
  description?: string;
}

export interface MonitorDeployment {
  as_of: number;
  stale: boolean;
  deployment: MonitorDeploymentSummary;
  alignment: MonitorAlignment | null;
  ttft_breakdown: MonitorTtftBreakdown | null;
  charts: MonitorDeploymentCharts;
  replicas: MonitorReplica[];
  // The full set of instance uuids the merged log stream should query - not
  // necessarily identical to `replicas` (e.g. a replica that has since
  // stopped or failed can still have logs worth showing).
  log_instances: string[];
  // Set only for a PD deployment (ADR 0028); an ordinary deployment never
  // carries this and the page never renders the PD card for it.
  pd?: MonitorDeploymentPd;
  // Where each signal the gateway can stand in for came from: the engine's
  // own series, or the gateway's when the engine reports none. A signal with
  // no data from either is absent. The page never labels the source; it uses
  // this to explain what only an engine can fill.
  sources?: Partial<Record<MonitorSignalSourceKey, MonitorSignalSource>>;
  // The deployment's health check.
  health: DeploymentHealth;
}

export interface MonitorSeriesParams {
  metric: string;
  by?: "node" | "deployment" | "instance" | "gpu";
  fn?: "avg" | "sum" | "max" | "rate";
  range?: MonitorRange;
  step?: string;
}

// monitorSeriesURL is exported so MetricExplorer can print the exact request
// URL it issued (plan §7 spec), without duplicating the query-building logic.
export function monitorSeriesURL(params: MonitorSeriesParams): string {
  const q = new URLSearchParams({ metric: params.metric });
  if (params.by) q.set("by", params.by);
  if (params.fn) q.set("fn", params.fn);
  if (params.range) for (const [k, v] of Object.entries(rangeParams(params.range))) q.set(k, v);
  if (params.step) q.set("step", params.step);
  return `/api/v1/monitor/series?${q.toString()}`;
}
