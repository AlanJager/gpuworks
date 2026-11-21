import type { Deployment } from "./deployments";

export interface DGPU {
  instance_uuid: string;
  deployment_id: number;
  deployment_name?: string;
  state: string;
  // 0 awake; a sleeper holds only its residue.
  sleep_level?: number;
  node_uuid: string;
  node_name: string;
  gpu_index: number;
  gpu_uuid?: string;
  gpu_name?: string;
  memory_limit_bytes: number;
  /** The deployment's limit when the slice still runs under another; it takes it at its next restart. */
  pending_limit_bytes?: number;
  sm_limit_pct?: number;
  memory_used_bytes?: number;
  sm_util_pct?: number;
  updated_at?: string;
  /** The slice's dGPU backend keeps no such reading, so none will arrive. */
  memory_used_unsupported?: boolean;
  sm_util_unsupported?: boolean;
}

/** An instance a dGPU quota change left at its limit until it restarts, and why. */
export interface DGPUQuotaPending {
  instance_uuid: string;
  reason: "backend" | "asleep" | "starting" | "in_use" | "no_usage";
  used_bytes?: number;
}

/** The answer to a dGPU quota change: what took the new limit at once and what waits for a restart. */
export interface DGPUQuotaResult {
  deployment: Deployment;
  live: string[];
  pending: DGPUQuotaPending[];
}

export interface DGPUListResponse {
  items: DGPU[];
}
