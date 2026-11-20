import type { StorageAddSuggestion } from "./pages/storage/storagePreset";
// The wire types of the Slurm cluster entries (GET/POST /api/v1/slurm/clusters,
// dto.SlurmCluster and its siblings). Every value that decides what the
// console offers (state, counts, whether the platform can apply or delete with
// the nodes, the missing nodes and why) is the server's; the console shows it.
import type { KindResource } from "./api";

/** Where an entry stands: revoked, stalled (no node joined for too long), pending (no node yet) or active. */
export type SlurmClusterState = "revoked" | "stalled" | "pending" | "active";

/** Why a GPU node Slurm lists has no agent online: never enrolled there, or enrolled and not connected. */
export type SlurmMissingReason = "not_enrolled" | "offline";

/** A GPU node Slurm lists that has no agent online (dto.SlurmMissingNode). */
export interface SlurmMissingNode {
  name: string;
  reason: SlurmMissingReason;
  /** Slurm's own words for the node, as reported. */
  slurm_state: string;
  slurm_reason?: string;
  /** The node's, for a node that joined and is offline: it can be removed (DELETE /api/v1/nodes/{uuid}). */
  node_uuid?: string;
}

/** Why an entry has no node yet, as the server reads it (dto.SlurmWaiting.reason). */
export type SlurmWaitingReason = "no_compute_nodes" | "server_unreachable" | "joining" | "no_node_called" | "not_applied";

/** One host's answer to whether it can reach the entry's server address (dto.SlurmReachHost). */
export interface SlurmReachHost {
  role: "controller" | "node";
  name: string;
  /** unknown: the host could not be asked, which is no failure. */
  status: "ok" | "failed" | "unknown";
  /** For failed, dns, connect, timeout, tls, http, no_curl or other; for unknown, not_asked, node_not_running, assistant_offline or command_failed. */
  reason?: string;
}

/** Why an entry with no node has none (dto.SlurmWaiting); hosts are those that cannot reach the server, for server_unreachable. */
export interface SlurmWaiting {
  reason: SlurmWaitingReason;
  hosts?: SlurmReachHost[];
}

/** Whether the server's address could be reached from the controller and the GPU compute nodes (dto.SlurmReach). */
export interface SlurmReach {
  hosts: SlurmReachHost[];
  /** Why the cloud's compute nodes could not be listed to ask them: permission_missing (with the action in nodes_permission) or failed. */
  nodes_unlisted?: "permission_missing" | "failed";
  nodes_permission?: string;
}

/** The cloud account cluster an entry was made from (dto.SlurmClusterSource). */
export interface SlurmClusterSource {
  provider_id: number;
  /** Empty when the account is no longer listed. */
  provider_name: string;
  provider_kind: string;
  provider_cluster_id: string;
}

/** Why the platform cannot set an entry up or remove it (dto.SlurmCluster.OperationBlocked). */
export type SlurmOperationBlocked = "no_source" | "no_provider" | "provider_unsupported" | "cluster_unknown" | "cluster_listing_pending" | "cluster_listing_failed" | "not_slurm_cluster" | "revoked";

/** Why deleting an entry together with its nodes is not possible now: an agent is connected, or a node has instances. */
export type SlurmDeleteBlocker = "online" | "instances";

/** A Slurm cluster whose GPU nodes join through a health check in its slurm.conf (dto.SlurmCluster). */
export interface SlurmCluster {
  id: number;
  uuid: string;
  name: string;
  server_url: string;
  /** Slurm's own ClusterName, bound by the first node that enrolls; empty until then. */
  slurm_cluster_name: string;
  run_as: string;
  /** The account the agents' placeholder jobs are submitted as; empty is the agent's own. */
  account_user: string;
  install_dir: string;
  script_dir: string;
  sbatch_args: string;
  idle_return_seconds: number;
  pending_timeout_seconds: number;
  revoked: boolean;
  state: SlurmClusterState;
  stalled: boolean;
  /** The nodes that enrolled. */
  node_count: number;
  /** The enrolled nodes whose agent is online; the others are offline. */
  online_count: number;
  /** Set while no agent of the entry is online: its list of GPU nodes is the one from before, at most this new. */
  list_as_of?: string;
  /** The GPU nodes Slurm lists, as an enrolled agent last reported them (0 before the first report). */
  slurm_gpu_nodes: number;
  /** The listed nodes with no agent online. */
  missing_count: number;
  /** The first of them, by name (at most the dictionary's missing_nodes_limit). */
  missing_nodes: SlurmMissingNode[];
  /** Why no node has joined, while none has and the entry is not revoked; absent when the server cannot tell. */
  waiting?: SlurmWaiting;
  /** The server suggests registering the cluster's shared filesystem as model storage, with what to fill the add-storage form with. */
  shared_storage_hint?: StorageAddSuggestion;
  category: string;
  resources: KindResource[];
  source?: SlurmClusterSource;
  created_at: string;
  /** The platform can run the setup on the cluster's controller itself (check and apply). */
  can_apply: boolean;
  /** It can take the setup out of the cluster again (a revoked entry still can). */
  can_remove: boolean;
  /** Why it cannot, as a code: no_source, no_provider, provider_unsupported, cluster_unknown, cluster_listing_pending, cluster_listing_failed, not_slurm_cluster or revoked (only applying is refused). */
  operation_blocked?: SlurmOperationBlocked;
  /** Deleting the entry together with its nodes would succeed now; when it would not, the blocker says why. Meaningful while node_count > 0. */
  delete_with_nodes_allowed: boolean;
  delete_with_nodes_blocker?: SlurmDeleteBlocker | "";
  /** The commands carry the enrollment token: administrators only. */
  setup_command?: string;
  /** The same command in check-only mode: it reports what the setup would do and changes nothing. */
  check_command?: string;
  remove_command?: string;
  uninstall_agents_command?: string;
}

/** What the cluster form takes from the server (dto.SlurmDefaults). */
export interface SlurmDefaults {
  run_as: string;
  install_dir: string;
  script_dir: string;
  idle_return_seconds: number;
  idle_return_min_seconds: number;
  idle_return_max_seconds: number;
  pending_timeout_seconds: number;
  pending_timeout_min_seconds: number;
  pending_timeout_max_seconds: number;
  health_check_interval_seconds: number;
  stalled_after_seconds: number;
  /** How often an agent reports and so picks up an edit. */
  report_interval_seconds: number;
  missing_nodes_limit: number;
  /** The platform's own temporary public address (a Cloudflare quick tunnel) while one is on, "" when none: a server URL that is it changes with the connector. */
  quick_tunnel_url: string;
}

/** Creates an entry; an omitted optional field takes the server's default (dto.SlurmClusterRequest). */
export interface SlurmClusterRequest {
  name: string;
  server_url: string;
  run_as?: string;
  account_user?: string;
  install_dir?: string;
  script_dir?: string;
  sbatch_args?: string;
  idle_return_seconds?: number;
  pending_timeout_seconds?: number;
  /** The cloud account's HPC cluster the entry is made from; both or neither. */
  provider_id?: number;
  provider_cluster_id?: string;
}

/** Edits an entry (dto.SlurmClusterUpdate): omitted keeps the value, an empty string clears (the name excepted). */
export interface SlurmClusterUpdate {
  name?: string;
  account_user?: string;
  sbatch_args?: string;
  idle_return_seconds?: number;
  pending_timeout_seconds?: number;
}

/** The answer to an edit (dto.SlurmClusterEdit): the entry, and the JSON names of the fields that changed, sorted. */
export interface SlurmClusterEdit {
  cluster: SlurmCluster;
  changed: string[];
}

/** The apply steps in the order they run, and how each ended (dto.SlurmApplyStep). */
export type SlurmApplyStepID = "gres" | "check_script" | "distribute" | "slurm_conf" | "reconfigure" | "resume" | "reach";
export type SlurmApplyStepStatus = "pass" | "warn" | "skip" | "fail";

export interface SlurmApplyStep {
  id: SlurmApplyStepID;
  status: SlurmApplyStepStatus;
}

/** What became of gres.conf, by node name (dto.SlurmApplyGres). */
export interface SlurmApplyGres {
  added: string[];
  present: string[];
  /** Nodes the cloud account lists with a card whose Gres lists no gpu in Slurm: nothing was added, slurm.conf is theirs to fix. */
  no_gpu_gres: string[];
}

/** How the check script reached the compute nodes (dto.SlurmApplyDistribute). */
export interface SlurmApplyDistribute {
  shared: boolean;
  ok: number;
  failed: string[];
}

/** POST /slurm/clusters/{uuid}/apply finishing (dto.SlurmApplyOutcome): "partial" is finished with something left to do by hand. */
export interface SlurmApplyOutcome {
  status: "ok" | "partial";
  manager_instance: string;
  steps: SlurmApplyStep[];
  gres: SlurmApplyGres;
  distribute: SlurmApplyDistribute;
  resumed_nodes: string[];
  /** Nodes that were to be resumed and could not be. */
  resume_failed_nodes: string[];
  reach: SlurmReach;
  cluster: SlurmCluster;
}

/** A gres.conf line a setup would append (dto.SlurmCheckGresLine): `NodeName=<node> Name=gpu File=<files>`. */
export interface SlurmCheckGresLine {
  node: string;
  files: string;
  /** The line as written to gres.conf. */
  line: string;
}

/** A HealthCheck setting of slurm.conf a setup would keep as a comment (dto.SlurmCheckComment). */
export interface SlurmCheckComment {
  line: number;
  key: string;
  value: string;
}

/** What a warning of a check names (dto.SlurmApplyCheck.warnings): each makes the setup end partial. */
export type SlurmCheckWarning = "conf_include" | "program_not_in_conf" | "not_configless" | "no_gpu_gres" | "server_unreachable";

/**
 * POST /slurm/clusters/{uuid}/apply with dry_run true (dto.SlurmApplyCheck): what a
 * setup would do, read now with nothing changed. Every value is the server's reading.
 */
export interface SlurmApplyCheck {
  dry_run: true;
  status: "ok" | "partial";
  manager_instance: string;
  /** The cluster's own ClusterName. */
  slurm_cluster: string;
  gres: SlurmApplyGres & { would_add: SlurmCheckGresLine[] };
  script: {
    check_script: string;
    token_file: string;
    shared: boolean;
    fstype: string;
    /** When the directory is not shared: the nodes the files would be copied to, and the files. */
    copy_to: string[];
    copy_files: string[];
  };
  conf: {
    path: string;
    /** false: slurm.conf is already as the setup would write it. */
    changed: boolean;
    interval: number;
    node_state: string;
    /** The settings of the block it would add, as written. */
    block: string[];
    chained_program?: string;
    chained_state?: string;
    commented: SlurmCheckComment[];
    includes: string[];
    unnamed_program?: string;
  };
  configless: boolean;
  rearm: boolean;
  resume_nodes: string[];
  warnings: SlurmCheckWarning[];
  reach: SlurmReach;
  cluster: SlurmCluster;
}

/** The uninstall steps of an entry the platform can reach, in order. */
export type SlurmRemoveStep = "uninstall_agents" | "remove_check";

/** The answer to a remove step (dto.SlurmRemoveOutcome): how long to wait before the next one, and which it is ("" after the last). */
export interface SlurmRemoveOutcome {
  step: SlurmRemoveStep;
  status: "ok" | "noop";
  manager_instance: string;
  wait_seconds: number;
  next_step?: SlurmRemoveStep | "";
  cluster: SlurmCluster;
}
