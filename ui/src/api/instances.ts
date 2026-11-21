import type { ProductMessage } from "../apiError";
import type { PdRoleName } from "./engines";
import type { ProductWarning } from "./scheduling";

/** The compute network a multi-node rank started on, recorded at launch. */
export interface InstanceComputeNetwork {
  name: string;
  management: boolean;
  type: "host_ethernet" | "host_rdma" | "accelerator_rdma" | "accelerator_hccs";
  interface: string;
  ip: string;
  link_layer?: "infiniband" | "roce" | "hccs";
}

// --- VRAM breakdown -------------------------------------------------------
export interface Breakdown {
  weight_bytes: number;
  per_dtype?: Record<string, number>;
  kv_bytes_per_token: number;
  kv_total_bytes: number;
  // One figure, not a decomposition: gpuworks does not model an engine's
  // internals, so the reserve is a single conservative constant.
  reserve_bytes: number;
  mm_encoder_bytes: number;
  per_gpu_bytes: number;
  startup_fixed_bytes?: number;
  total_bytes: number;
  actual_total_bytes?: number;
  grade: string;
  tensor_parallel: number;
  max_model_len: number;
  source?: "estimate" | "measured";
  gpu_memory_utilization?: number;
  capacity_bytes?: number;
  kv_pool_bytes?: number;
  estimated_kv_tokens?: number;
  kv_estimate_confidence?: string;
  kv_estimate_reason?: string;
  fits: boolean;
  max_concurrency?: number;
  vram_limit_bytes?: number;
}
export interface Measured {
  ModelLoadingSeconds?: number;
  CompilationSeconds?: number;
  GraphCaptureSeconds?: number;
  StartupSeconds?: number;
  ModelWeightsGiB: number;
  KVCacheGiB: number;
  KVCacheTokens: number;
  GraphCaptureGiB: number;
  MaxConcurrency: number;
  NumGPUBlocks: number;
  Found: boolean;
}

export interface RuntimeMount {
  source?: string;
  volume?: string;
  target: string;
  read_only?: boolean;
  size_hint?: string;
  purpose?: string;
}

export interface StartupEstimate {
  weight_bytes: number;
  method: string;
  estimated_seconds?: number;
  reference_mib_per_sec?: number;
  initialization_seconds?: number;
}

export interface Instance {
  pre_start_argv?: string[];
  pre_start_script?: string;
  start_timeout_sec?: number;
  start_timeout_source?: string;
  start_timeout_conflict?: boolean;
  engine_start_timeout_sec?: number;
  startup_estimate?: StartupEstimate;
  uuid: string;
  deployment_id: number;
  node_uuid: string;
  state: string;
  gpu_indexes: number[];
  // The dGPU memory limit on each of its GPUs; absent for whole cards.
  vram_limit_bytes?: number;
  ports: number[];
  api_port: number;
  image: string;
  image_digest_id?: string;
  message: string;
  runtime_failure?: ProductMessage;
  // notes are non-blocking facts the server derives about the instance
  // (code + params), e.g. a card shared with processes outside the platform.
  notes?: ProductWarning[];
  container_id?: string;
  phase?: string;
  progress?: number;
  bytes_done?: number;
  bytes_total?: number;
  rate_bps?: number;
  model_progress?: {
    state: string;
    progress: number;
    size_bytes: number;
    downloaded: number;
    transferred_bytes: number;
    rate_bps: number;
    message?: string;
  };
  sleep_level: number;
  // The level the instance is ordered to (0 awake); it differs from
  // sleep_level while the order is being carried out.
  desired_sleep?: number;
  // The server's verdicts about the instance, which the UI shows as they are:
  // asleep (the gateway sends it nothing because of sleep), the replica it
  // fills (absent when it fills none), and whether it takes a sleep or wake
  // order of its own.
  asleep?: boolean;
  // sleep_held: an operator's or a schedule's sleep holds this instance, so
  // a request does not wake it.
  sleep_held?: boolean;
  replica?: { role?: string; index: number };
  // A retained failed attempt superseded by a later failure or a filled replica.
  previous_attempt?: boolean;
  can_sleep_alone?: boolean;
  can_wake_alone?: boolean;
  // A rank of a multi-node replica: rank 0 leads and serves, the rest are
  // headless followers (dist_role, normalized by the server for old rows).
  // dist_group + dist_attempt identify the generation the ranks were
  // launched as; nnodes is the replica's rank count.
  node_rank?: number;
  headless?: boolean;
  nnodes?: number;
  dist_role?: "leader" | "follower" | string;
  dist_group?: string;
  dist_attempt?: number;
  schedule?: { explain?: any; breakdown?: Breakdown; network?: { compute?: InstanceComputeNetwork }; unit?: { key?: string } };
  measured?: Measured;
  entrypoint?: string[];
  argv?: string[];
  env?: Record<string, string>;
  docker_args?: string[];
  /** Size of the container's /dev/shm tmpfs, in bytes. */
  shm_bytes?: number;
  mounts?: RuntimeMount[];
  command_mode?: string;
  model_mount_path?: string;
  // The model files the instance reads, as recorded when it launched: the
  // storage each is in (absent once that storage is gone) and the host path.
  model_files?: InstanceModelFile[];
  // role is the instance's PD role; absent outside PD deployments.
  role?: PdRoleName | string;
  // kv_offload_backend is the KV offload backend the instance keeps, as the
  // server scoped the deployment's selection to the instance's PD role:
  // absent when it keeps the engine's own tiers or none.
  kv_offload_backend?: { id: string; label: string };
}

export interface InstanceModelFile {
  model_id: number;
  storage_id?: number;
  storage_name?: string;
  storage_type?: string;
  path: string;
}

export interface LaunchFactsIntent {
  deployment_id: number;
  name: string;
  engine: string;
  image?: string;
  served_name?: string;
  desired_state: string;
  replicas: number;
  env: Record<string, string>;
  sleepable: boolean;
  desired_sleep: number;
  vram_limit_bytes: number;
  sm_limit_pct: number;
  partition_backend?: string;
  nnodes?: number;
  node_rank?: number;
}

export interface LaunchFactsMount {
  source?: string;
  volume?: string;
  target: string;
  read_only?: boolean;
  size_hint?: string;
  purpose?: string;
}

export interface LaunchFactsResolved {
  command_mode?: string;
  entrypoint?: string[];
  argv: string[];
  pre_start_argv?: string[];
  pre_start_script?: string;
  env?: Record<string, string>;
  docker_args?: string[];
  mounts?: LaunchFactsMount[];
  model_mount: string;
  health_path?: string;
  placement_variables?: Record<string, unknown>;
}

export interface RuntimeObservationMount {
  type?: string;
  source?: string;
  destination: string;
  read_only?: boolean;
}

export interface RuntimeContainerObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  container_id?: string;
  image_id?: string;
  entrypoint: string[];
  cmd: string[];
  path?: string;
  args: string[];
  env: Record<string, string>;
  mounts: RuntimeObservationMount[];
  devices: string[];
  runtime_name?: string;
  network_mode?: string;
}

export interface RuntimeImageObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  image_id?: string;
  entrypoint: string[];
  cmd: string[];
  env: Record<string, string>;
  volumes: string[];
}

export interface RuntimeProcessObservation {
  status: "observed" | "unknown" | string;
  reason?: string;
  observed_at_unix_ms?: number;
  pid?: number;
  identity?: string;
  argv?: string[];
  env?: Record<string, string>;
}

export interface InstanceLaunchFacts {
  intent: {
    status: "available" | "unknown" | string;
    value?: LaunchFactsIntent;
  };
  resolved: {
    status: "available" | "unknown" | string;
    value?: LaunchFactsResolved;
  };
  runtime_observation_relation: {
    status: "current" | "historical" | "unverified" | "unknown" | string;
    reason?: string;
  };
  runtime_observation: {
    runtime: string;
    container: RuntimeContainerObservation;
    image: RuntimeImageObservation;
    process: RuntimeProcessObservation;
  };
  runtime_observation_received_at?: string;
}

export interface InstanceEvent {
  /** Pagination cursor: pass the last entry's id back as `before`. */
  id: number;
  kind: string;
  code?: string;
  params?: Record<string, unknown>;
  message?: string;
  data?: string;
  created_at: string;
}
