import type { Eligibility } from "../deploymentCapabilities";
import type { DiagnosticReport } from "../diagnosticChecks";
import type { PartitionCapability } from "./gpus";
import type { PdRoleName } from "./engines";
import type { AcquisitionResult } from "./acquisition";
import type { ProductWarning, ScheduleExplain, ScheduleMessageLike, SchedulePlan, ScheduleReason } from "./scheduling";
import type { Breakdown, RuntimeMount } from "./instances";

// One dictionary flag the server took out of extra_args and put in its field.
// The form follows that move instead of parsing arguments of its own: the
// launch configuration has a single typed representation (ADR 0020).
export interface MovedLaunchFlag {
  key: string;
  flag: string;
  value: unknown;
  source: "extra_args";
}

export interface EvaluateFieldError {
  // The form field the error belongs beside: a dictionary key, a wire field
  // name, or container_mount_rows for mount rows.
  field: string;
  code: string;
  params?: Record<string, unknown>;
}

export interface EvictionSuggestion {
  instance_uuid: string;
  deployment_id: number;
  deployment: string;
  gpu_indexes: number[];
  frees_gib: number;
}

export interface EligibleNode {
  // Explicit node conditions, independently of vendor/chip/state checks.
  requirements_match?: boolean;
  uuid: string;
  name: string;
  eligible: boolean;
  reasons?: ScheduleMessageLike[];
}

export interface WeightSourceOption {
  provider: string;
  repo_id: string;
  revision?: string;
  subfolder?: string;
  suggested: boolean;
  registered: boolean;
}

/** One inline preflight check (ADR 0067): id names the check (a per-node
 * check repeats it with params.node), status is one of the five check
 * statuses, code + params render the text, message is the fallback. */
export interface PreflightCheck {
  id: string;
  status: string;
  code: string;
  params?: Record<string, unknown>;
  message?: string;
  evidence?: string;
  blocked_by?: string[];
}

export interface PreflightSummary {
  ok: boolean;
  checks?: PreflightCheck[];
}

export interface EvaluateResponse {
  gpu_candidates?: GPUCandidates;
  /** The instance start timeout the draft resolves to, and where it came from
   * (cluster, template, engine or deployment); absent when not resolved. */
  start_timeout_sec?: number;
  start_timeout_source?: string;
  schema_version?: number;
  diagnostics?: DiagnosticReport;
  request_id?: string;
  // Request errors attributed to form fields. A response with errors has
  // ok=false and stays HTTP 200; the codes match the create 400 path.
  errors?: EvaluateFieldError[];
  // Every node judged against this draft, eligible or not.
  eligible_nodes?: EligibleNode[];
  // The dGPU backend the eligible nodes offer (the first ready one); absent
  // when none has one. An automatic draft gates its quota on it.
  pool_partition_capability?: PartitionCapability;
  // What the form should display: the effective launch configuration with the
  // dictionary defaults and template projection merged, plus extra_args after
  // the moved flags left it.
  form_values?: Record<string, unknown>;
  // The form's former hardcoded constants, answered by the server.
  defaults?: { model_mount: string; master_port: number; engine: string };
  weight_sources?: WeightSourceOption[];
  preflight?: PreflightSummary;
  /** The split the draft launches with (server-computed; absent until known). */
  parallelism?: { tp: number; pp: number; nodes: number; gpus_per_node: number };
  plan_scope?: "per_replica" | string;
  replicas?: number;
  pre_start_argv?: string[];
  pre_start_script?: string;
  acquisition?: AcquisitionResult;
  ok: boolean;
  // The server picked the nodes of this multi-node or PD request
  // (placement_mode "auto"): the nodes shown are where it would go now, and the
  // create picks again.
  auto_placed?: boolean;
  // The complete launch configuration after materialization: every dictionary
  // key the deployment will start with, defaults included.
  effective_params: Record<string, unknown>;
  // Dictionary flags the server moved out of extra_args into their fields.
  moved?: MovedLaunchFlag[];
  // The dictionary keys the selected catalog spec contributed, and the tokens
  // of that spec which are outside the dictionary and stay verbatim.
  template_params?: Record<string, unknown>;
  template_residual_args?: string[];
  // Keys whose default the effective launch leaves to the engine: the form
  // starts them at "automatic" and the deployer may still turn them on or off.
  engine_decides_param_keys?: string[];
  skipped?: boolean;
  skip_reason?: string;
  template_spec_source?: string;
  template_spec_name?: string;
  template_match?: "spec-name" | "automatic" | string;
  matched_by?: string;
  template_deviated: boolean;
  entrypoint?: string[];
  argv: string[];
  command?: string[];
  env?: Record<string, string>;
  command_mode?: string;
  model_mount_path?: string;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  env_layers?: {
    system?: Record<string, string>;
    template?: Record<string, string>;
    user?: Record<string, string>;
  };
  rank_commands?: EvaluateRankCommand[];
  image: string;
  warnings: ProductWarning[];
  breakdown?: Breakdown;
  eviction?: EvictionSuggestion[];
  selected_node?: {
    uuid: string;
    name?: string;
    reasons?: ScheduleMessageLike[];
  };
  candidates?: Array<{
    node_uuid: string;
    node_name?: string;
    ok: boolean;
    reason?: ScheduleMessageLike;
    plan?: {
      ok: boolean;
      message: ScheduleMessageLike;
      plan?: SchedulePlan;
      explain?: ScheduleExplain;
    };
  }>;
  plan?: {
    ok: boolean;
    message: ScheduleMessageLike;
    plan?: SchedulePlan;
    explain?: ScheduleExplain;
  };
  // PD previews a PD disaggregated draft instance by instance (ADR 0028); the
  // command fields above then describe the first decode instance.
  pd?: EvaluatePd;
  // The shell script a multi-node or PD draft's manual command starts from,
  // written with platform variables the server expands per rank. Absent for a
  // single launch and when no faithful script exists (MANUAL_DRAFT_UNAVAILABLE).
  manual_command_draft?: string;
  // The prefixed platform variable names these launches state, sorted.
  manual_command_variables?: string[];
  // Whether the draft may be created with a yield policy other than never,
  // the create's own check; absent until the draft's engine is known.
  yield_policy_eligible?: Eligibility;
  // Whether a deployment created from this draft can have a health check, the
  // create's own rule on the contract the deployment will get (a template
  // draft's includes the template's operations); absent until the draft's
  // model is resolved.
  health_checkable?: boolean;
  // Whether a deployment of this draft may run its engine with per-request
  // priority scheduling, judged on the arguments the engine will receive;
  // absent until the draft's engine is known.
  priority_scheduling?: PrioritySchedulingEligibility;
}

// PrioritySchedulingEligibility is the server's answer for the priority
// scheduling switch: a refusal carries its product code with params, and
// fix_args when adding those arguments clears it.
export interface PrioritySchedulingEligibility {
  ok: boolean;
  refusal?: ProductWarning;
  fix_args?: string[];
}

export interface GPUCandidates {
  node_uuid: string;
  // note is what choosing an available card entails (a schedule reason
  // code), e.g. sharing it with processes outside the platform.
  items: { index: number; available: boolean; reason?: ScheduleReason; note?: ScheduleReason }[];
}

export interface EvaluatePdUnit {
  node_uuid: string;
  node_name?: string;
  address?: string;
  gpus?: number[];
  fits: boolean;
  reason?: ScheduleMessageLike;
  breakdown?: Breakdown;
  entrypoint?: string[];
  argv?: string[];
  command?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  // The unit's headless ranks on other nodes, in rank order - same shape as
  // a unit itself (node_uuid, node_name, address, gpus, fits, reason, argv,
  // command, env, docker_args, mounts).
  followers?: EvaluatePdUnit[];
}

export interface EvaluatePdRole {
  role: PdRoleName;
  tp: number;
  // Pipeline-parallel degree; absent when the role runs PP 1.
  pp?: number;
  units: EvaluatePdUnit[];
}

export interface EvaluatePd {
  mode: string;
  fallback: string;
  kv_load_failure: string;
  // Decode first, then prefill (ADR 0028's role order).
  roles: EvaluatePdRole[];
}

export interface EvaluateRankCommand {
  pre_start_argv?: string[];
  pre_start_script?: string;
  node_uuid: string;
  node_name?: string;
  rank_address?: string;
  node_rank: number;
  role: "standalone" | "leader" | "follower";
  headless: boolean;
  image: string;
  entrypoint?: string[];
  argv: string[];
  command?: string[];
  env?: Record<string, string>;
  command_mode?: string;
  model_mount_path?: string;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  env_layers?: {
    system?: Record<string, string>;
    user?: Record<string, string>;
  };
}

/** One directed pair of the network preflight, as a check result: status is
 * pass or fail; evidence is the agent's own text, never the message. */
export interface DistributedPreflightPair {
  id: string;
  status: string;
  code: string;
  params?: Record<string, unknown>;
  message?: string;
  source_uuid: string;
  target_uuid: string;
  target: string;
  reachable: boolean;
  rtt_ms_p50: number;
  rtt_ms_p99: number;
  gbps?: number;
  bandwidth_tested: boolean;
  bytes_sent?: number;
  bytes_received?: number;
  duration_seconds?: number;
  evidence?: string;
  bandwidth_evidence?: string;
}

export interface DistributedPreflightResponse {
  ok: boolean;
  pairs: DistributedPreflightPair[];
}
