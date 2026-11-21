import type { AgentVersionChange } from "../agentUpgradesTypes";
import type { ProductMessage } from "../apiError";
import type { List, ListPage, ListQuery } from "./http";
import type { PlatformResource } from "./platform";
import type { GPU, PartitionCapability, Topology } from "./gpus";
import type { ScheduleMessage } from "./scheduling";

// SystemLabel is one generated node label. value is what a selector matches
// on; display is the text to show, and display_code names a translation to
// prefer when the value is platform vocabulary rather than someone's name.
export interface SystemLabel {
  key: string;
  value: string;
  display: string;
  display_code?: string;
}

export interface RequirementLabel extends SystemLabel {
  nodes: number;
}

export type NodeScheduling = "schedulable" | "cordoned" | "draining";

// NodeReplica is one instance on a node and whether a drain moves it: a
// "pool" replica is replaced on another node, a "pinned" one waits (its
// deployment names only this node), a "named" multi-node or PD rank moves
// only when evicted.
export interface NodeReplica {
  instance: string;
  deployment_id: number;
  deployment: string;
  state: string;
  kind: "pool" | "pinned" | "named";
  drain_reason?: ScheduleMessage;
  drain_reason_at?: string;
  drain_message?: string;
  /** The failed attempt's structured reason; absent for an older attempt. */
  drain_failure?: ProductMessage;
}

export interface NodeReplicas {
  drain_offline?: boolean;
  scheduling: NodeScheduling;
  items: NodeReplica[];
}

/** One vendor whose cards are in a node's host (Node.accelerator_vendors). */
export interface AcceleratorPresence {
  vendor: string;
  cards: number;
  model?: string;
}

/** How strictly a node's NVIDIA instance pods are kept to their own card (Node.gpu_isolation). */
export interface NodeGPUIsolation {
  /** "soft". */
  level: string;
  /** Why: cdi_names_missing. */
  reason: string;
  /** gpu_operator or host when the agent could tell whose toolkit it is. */
  toolkit?: string;
  /** How many NVIDIA cards no CDI spec names. */
  cards?: number;
  /** Ways back to strict isolation; run each fix's commands in order, as written. */
  fixes: Array<{ code: string; commands: string[] }>;
}

/** A node whose kernel hangs UCX, and so NIXL, the PD KV transport (Node.ucx_netlink_hang). */
export interface NodeUCXNetlinkHang {
  /** The node's kernel release. */
  kernel?: string;
  /** Ways to stop it (disable_ipv6_now, disable_ipv6_persist); run each fix's commands in order, as written. */
  fixes: Array<{ code: string; commands: string[] }>;
}

/** A container daemon socket the node agent's account may not open (Node.runtime_fallback_socket). */
export interface RuntimeSocketDenial {
  /**
   * The server's reading of the facts, which words the notice:
   * not_in_group | restart_pending | group_held | root_group | unknown.
   */
  cause: string;
  socket: string;
  /** The group that owns the socket; absent when its gid has no name. */
  group?: string;
  /** Absent when the agent could not read the socket. */
  gid?: number;
  /** The account the agent runs as; absent when it could not tell. */
  account?: string;
  /** Another group of the same name the account is in (a directory service's). */
  same_name_gid?: number;
  /** systemd | nohup, absent when the agent cannot tell what restarts it. */
  supervisor?: string;
  /** Adds the account to the group and restarts the agent where the supervisor is known. */
  command?: string;
}

/** One step that brings a Slurm node back to running jobs (dto.NodeSlurmFix). */
export interface NodeSlurmFix {
  code: string;
  commands: string[];
}

export interface Node {
  uuid: string;
  // scheduling is whether the node takes new replicas: cordoned takes none,
  // draining also moves the replicas a pool placed there.
  scheduling?: NodeScheduling;
  // scheduling_by is who set scheduling (absent when nobody holds the node,
  // or the author was not recorded): operator, platform, or the source of
  // an extension that holds the node. Each lifts only its own.
  scheduling_by?: string;
  // cordon_reason is why the platform holds the node now; absent unless
  // scheduling_by is platform.
  cordon_reason?: "runtime_gpu_blocked" | "card_needs_host_reboot" | "kube_unschedulable" | "slurm_node_unavailable";
  // cordon_message is why an extension holds the node out of scheduling;
  // absent when none does.
  cordon_message?: ProductMessage;
  // uncordon is what making the node schedulable does, decided by the
  // server: allowed; confirm, when the platform will cordon it again on its
  // next report; refused, while an extension holds it.
  uncordon?: "allowed" | "confirm" | "refused";
  /** The node's name in Slurm, while its agent borrows the node's cards from Slurm. */
  slurm_node?: string;
  /** Why Slurm will not start jobs on the node; absent when it will. */
  slurm_unavailable?: string;
  /** Slurm's own Reason text for that state, shown as written. */
  slurm_reason?: string;
  /** The steps that bring the node back, in order, built by the server: text by code, commands as written. */
  slurm_fixes?: NodeSlurmFix[];
  name?: string;
  token?: string;
  /** Installs this node's agent; returned with the token, and the way back
   *  for a node the platform cannot restore over SSH. */
  install_command?: string;
  /** Whether the agent is on the other end; see nodes/connectionState. */
  state: "online" | "offline" | "connecting";
  hostname: string;
  os: string;
  kernel: string;
  arch: string;
  cpu_model: string;
  cpu_cores: number;
  mem_total_gib: number;
  disk_total_gib: number;
  gpu_count: number;
  /** Why the last accelerator probe failed, and when; absent once one succeeds. */
  gpu_probe_error?: string;
  /**
   * Why this node's container runtime cannot give a container its
   * accelerators. While set, the platform keeps the node cordoned.
   */
  runtime_gpu_blocked?: string;
  /**
   * Display name of the vendor container plugin this node's vendor and
   * runtime need ("NVIDIA Container Toolkit"); empty when none is needed.
   */
  runtime_gpu_plugin?: string;
  /**
   * The runtime the agent passed over for runveil ("docker" | "nerdctl"),
   * why (gpu_plugin_missing | gpu_runtime_unconfigured | daemon_not_ready |
   * socket_permission_denied | gpu_ready), and the plugin that runtime
   * needs. Absent when nothing was passed over.
   */
  runtime_fallback_from?: string;
  runtime_fallback_reason?: string;
  runtime_fallback_plugin?: string;
  /**
   * With socket_permission_denied: the daemon socket the agent's account
   * may not open, as the agent found it, and the server's command that adds
   * the account to the group and restarts the agent.
   */
  runtime_fallback_socket?: RuntimeSocketDenial;
  /**
   * Set while a Kubernetes node's NVIDIA instance pods are kept to their own
   * card by CUDA_VISIBLE_DEVICES alone (soft isolation), with the fixes the
   * server offers and their literal commands. Absent when strict.
   */
  gpu_isolation?: NodeGPUIsolation;
  // ucx_netlink_hang is set while the node's kernel hangs UCX (NIXL): PD
  // disaggregation over NIXL cannot start on it.
  ucx_netlink_hang?: NodeUCXNetlinkHang;
  /**
   * When the current streak of failed vendor-tool probes started; kept
   * across consecutive failures and absent once a probe succeeds.
   */
  gpu_probe_failed_at?: string;
  gpu_vendor?: string;
  gpu_driver?: string;
  nvidia_driver?: string;
  framework?: string;
  framework_version?: string;
  path_usage?: Array<{
    kind: string;
    /** Names the storage a storage or storage_cache entry measures. */
    storage_id?: number;
    path: string;
    total_bytes: number;
    available_bytes: number;
  }>;
  runtime_driver?: string;
  runtime_preference?: string;
  /** The runtime preferences the operator may set, in the order to offer; absent when the node has no choice (a Kubernetes node). */
  runtime_choices?: string[];
  /** The operator's choice of which vendor's cards the node manages: auto or a vendor; absent is auto. */
  accelerator_preference?: string;
  /** Every vendor whose cards the agent saw on the host's bus, the managed one included. */
  accelerator_vendors?: AcceleratorPresence[];
  /** "<Name> <version>" of the installed vendor container plugin; empty when it is not installed. */
  runtime_plugin?: string;
  runtimes?: string[];
  partition_capabilities?: PartitionCapability[];
  labels?: Record<string, string>;
  // The labels the platform generates from the node's own facts, kept apart
  // from the written ones because they cannot be edited. A key appears once
  // per value the node carries for it.
  system_labels?: SystemLabel[];
  /** The external platform the node joined through; absent for a node added by hand. */
  source?: NodeSource;
  /** Where the node came from could not be read: no source without the node having been added by hand. */
  source_unknown?: boolean;
  // The address the node's agent reaches the server from: its address on the
  // management network.
  management_address?: string;
  server_url?: string;
  connect_mode?: string;
  server_dial_address?: string;
  server_dial_listen?: string;
  agent_version?: string;
  agent_eligibility?: string;
  agent_version_change?: AgentVersionChange;
  caps?: string[];
  last_remote_addr?: string;
  last_reject_reason?: string;
  last_reject_at?: string;
  last_seen_at?: string;
  recovery_phase?:
    | ""
    | "probing"
    | "artifact_check"
    | "uploading"
    | "verifying"
    | "replacing"
    | "restarting"
    | "reinstalling"
    | "reconnecting"
    | "unreachable"
    | "recovery_blocked"
    | "stopped";
  recovery_attempts?: number;
  last_probe_at?: string;
  last_recovery_error?: string;
  /** Names last_recovery_error (SSH_*, AGENT_INSTALL_*, RECOVERY_*); absent on errors recorded before codes. */
  last_recovery_code?: string;
  has_ssh_credential: boolean;
  created_at: string;
  gpus: GPU[];
  topology?: Topology;
  /** Resolved compute network and its bootstrap IP (multi-node rank address). */
  compute_network?: string;
  compute_address?: string;
}

export interface AgentRejectEvent {
  token_prefix: string;
  reason: string;
  remote: string;
  at: string;
}

export interface FSEntry {
  name: string;
  is_dir: boolean;
  size: number;
}

// FSListResponse is GET /nodes/{uuid}/fs: the directory listed, with its
// entries as a List alongside.
export interface FSListResponse extends List<FSEntry> {
  path: string;
}


export interface AgentFleetSummary {
  target: string;
  align: number;
  upgrade?: number;
  downgrade?: number;
  different_build?: number;
  check: number;
  offline?: number;
  run_id?: number;
}

/** A node page row omits topology and path usage (GET /nodes/{uuid} has them). */
export interface NodePage extends ListPage<Node> {
  agent_fleet?: AgentFleetSummary;
  /** Every node, whatever the filters; total counts the matches. */
  all: number;
  /** Every user label key=value across all nodes, whatever the filters. */
  labels: Array<{ key: string; values: string[] }>;
  /** Every source a node came from, whatever the filters: what the source filter offers. */
  sources: NodeSourceOption[];
}

/** One value of the node list's source filter (dto.NodeSourceOption): an account ("provider:<id>"), a cluster ("cluster:<uuid>") or "manual". */
export interface NodeSourceOption {
  value: string;
  provider_name?: string;
  provider_kind?: string;
  cluster_name?: string;
  nodes: number;
}

/** What GET /nodes/facets says about the nodes its filters match. */
export interface NodeFacets {
  total: number;
  labels: Array<{ key: string; values: Array<{ value: string; nodes: number }> }>;
  /** Generated labels except the node identity, with the server's display text. */
  system_labels: Array<{ key: string; values: Array<{ value: string; display: string; display_code?: string; nodes: number }> }>;
  /**
   * Distinct accelerator models, each with how many matched nodes have one;
   * model is the card's gpuworks.ai/gpu-model label value.
   */
  gpus: NodeGPUCount[];
}

/** One reported accelerator identity aggregated by GET /nodes/facets. */
export interface NodeGPUCount {
  vendor: string;
  name: string;
  cc: string;
  arch_id?: string;
  model: string;
  nodes: number;
  cards: number;
  memory_bytes: number;
}

/** The filters of GET /nodes/facets: a list query without the page. */
export type NodeFilter = Omit<ListQuery, "page" | "page_size">;

export function nodeFilterString(filter: NodeFilter): string {
  const params = new URLSearchParams();
  const q = filter.q?.trim();
  if (q) params.set("q", q);
  if (filter.state?.length) params.set("state", filter.state.join(","));
  for (const pair of filter.label ?? []) params.append("label", pair);
  return params.toString();
}

/** The external platform a node joined through (dto.NodeSource). */
export interface NodeSource {
  /** What the node is on the platform; empty when the platform's kind does not say. */
  resource: PlatformResource | "";
  /** The account; absent for a cluster connected on its own. */
  provider_id?: number;
  provider_name?: string;
  provider_kind?: string;
  /** The cluster entry of a cluster node. */
  cluster_uuid?: string;
  cluster_name?: string;
}
