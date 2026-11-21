import type { LocalizedText } from "../i18n/localizedText";
import type { List } from "./http";
import type { KindResource } from "./platform";
import type { CheckResult } from "./ssh";
import type { KubeVendor } from "./kube";

export type ProviderCategory = "public_cloud" | "private_cloud";

export interface CloudProvider {
  id: number;
  name: string;
  kind: string;
  // The platform the kind connects, which the server derives; the platform type column shows it.
  category: ProviderCategory;
  state: string;
  instance_count: number;
  /** What the account's kind connects, in the order to list it. */
  resources: KindResource[];
  /** Compute nodes that joined through the account, on its instances or through its clusters. */
  node_count: number;
  /** When the account's instances were last listed; absent before its first sync. */
  synced_at?: string;
  created_at: string;
  updated_at: string;
}

export interface ProviderEditConfig {
  id: number;
  name: string;
  kind: string;
  endpoint: string;
  public_key: string;
  extra: Record<string, string>;
}

export interface ProviderCapabilities {
  can_list_instances: boolean;
  can_power_control: boolean;
  can_install_agent: boolean;
  can_create: boolean;
  can_delete: boolean;
  can_inject_user_data: boolean;
  has_pricing: boolean;
}

export interface ProviderCredentialField {
  name: string;
  label: LocalizedText;
  secret: boolean;
  required: boolean;
  placeholder?: LocalizedText;
  default_value?: string;
  // Editable creation hint that must be changed before submission.
  default_template?: string;
  extra?: boolean;
}

export interface ProviderKindInfo {
  kind: string;
  label: LocalizedText;
  homepage?: string;
  capabilities: ProviderCapabilities;
  credential_fields: ProviderCredentialField[];
  default_server_dial_listen?: string;
  server_dial_supported: boolean;
  // The provider maps container ports: the server chooses the agent's port
  // and address when they are left empty.
  server_dial_auto_port?: boolean;
  /** The account's managed Kubernetes clusters can be listed and connected (GET /providers/{id}/clusters). */
  can_list_clusters?: boolean;
  /** The account's HPC clusters (E-HPC) can be listed and connected as Slurm entries (GET /providers/{id}/hpc-clusters). */
  can_list_hpc_clusters?: boolean;
  /** What the kind connects, in the order to list it. */
  resources: KindResource[];
  /** The platform type the kind's accounts are filed under; the platform choice is grouped by it. Empty for a kind that declares none. */
  category: ProviderCategory | "";
}

/** A platform type as the platform choice offers it (GET /providers/kinds categories), in order. */
export interface ProviderCategoryInfo {
  id: string;
  /** What the type connects, for a type that is no vendor's (Kubernetes). */
  resources?: KindResource[];
  /** Added as a Kubernetes cluster entry, with the cluster form, not as an account. */
  kube_cluster?: boolean;
  /** Added as another kind of cluster entry (slurm), with that kind's cluster form, not as an account. */
  cluster_kind?: "slurm";
  /** The kinds filed under the type, in the order of items. */
  kinds: string[];
}

/** GET /providers/kinds: the kinds an account can be added for, and the platform types to offer them under. */
export interface ProviderKindList extends List<ProviderKindInfo> {
  categories: ProviderCategoryInfo[];
}

/** A managed Kubernetes cluster of a cloud account (dto.ProviderCluster), listed live. */
export interface ProviderCluster {
  external_id: string;
  name: string;
  /** The cluster runs, has GPU node pools and no entry yet, so an entry can be made from it. */
  connectable: boolean;
  region: string;
  version: string;
  /** The vendor's product the cluster is (ack_managed, doks…) and its name; absent when the driver cannot tell. */
  type?: string;
  type_label?: LocalizedText;
  /** The vendor's own word for the cluster's state. */
  state: string;
  running: boolean;
  /** The API server has a public address, so gpuworks can apply the manifest itself. */
  public_api: boolean;
  gpu_pools: ProviderClusterPool[];
  /** The cluster entry already made from this cluster. */
  entry?: { uuid: string; name: string };
  /** The registry an entry made from it pulls the agent image from unless the form names another. */
  agent_image_source: string;
}

/** A GPU node pool of a ProviderCluster. */
export interface ProviderClusterPool {
  id: string;
  name: string;
  gpu_model: string;
  /** GPUs per node. */
  gpu_count: number;
  nodes: number;
  vendor: KubeVendor;
  /** The cluster's entry holds this pool: the DaemonSet runs on its nodes. */
  in_entry: boolean;
}

export type HPCClusterState = "uninit" | "creating" | "initing" | "pending" | "running" | "exception" | "stopping" | "stopped" | "releasing" | "unknown";

/** An HPC cluster (Alibaba Cloud E-HPC) of a cloud account (dto.ProviderHPCCluster), listed live. */
export interface ProviderHPCCluster {
  external_id: string;
  /** The cluster's ID when the cloud gave no name. */
  name: string;
  region: string;
  /** The cluster's workload manager, lower case: slurm, pbs… Empty when the cloud did not say. */
  scheduler: string;
  scheduler_version: string;
  /** The cluster's state as the server's stable code; the text is the client's. unknown: a word the cloud does not document (see vendor_state). */
  state: HPCClusterState;
  /** The badge colour the server gives the state. */
  state_tone: "success" | "info" | "neutral" | "danger";
  /** The cloud's own word for the state. */
  vendor_state: string;
  running: boolean;
  /** Compute nodes the cloud lists in the cluster now, in any state; absent when the listing gives no count. Not a GPU count. */
  compute_nodes?: number;
  /** Compute nodes with a GPU as the last sync listed them; only given when there is no compute_nodes. */
  gpu_nodes?: number;
  /** An entry can be made from it: it runs, schedules with Slurm and has no entry yet. */
  connectable: boolean;
  /** The platform can also run the setup on its manager node itself. */
  can_apply: boolean;
  /** Why a cluster with no entry cannot be connected at all, as the server decides: scheduler_not_slurm (only Slurm's cards the agent shares). */
  unsupported_reason?: "scheduler_not_slurm";
  /** Why a Slurm cluster with no entry cannot be connected yet because of its state, as the server decides; at most one of the two reasons is set. */
  not_connectable_reason?: "cluster_starting" | "cluster_pending" | "cluster_exception" | "cluster_stopped" | "cluster_releasing" | "cluster_state_unknown";
  /** The Slurm entry already made from this cluster. */
  entry?: { uuid: string; name: string };
}

/**
 * The account's E-HPC clusters and how far the cloud let it be listed
 * (dto.ProviderHPCClusterList). access "permission_missing" (permission is the
 * RAM action) and "not_activated" (code is the cloud's word) leave items empty
 * and are no error; partial says the clusters of failed_regions could not be
 * read, so a cluster of theirs may be missing.
 */
export interface ProviderHPCClusterList extends List<ProviderHPCCluster> {
  access: "ok" | "permission_missing" | "not_activated";
  permission?: string;
  code?: string;
  partial: boolean;
  failed_regions: Array<{ region: string; code: string; message: string; permission?: string }>;
  /** When the cloud was read for this listing. */
  listed_at?: string;
  /** A newer read is going on behind this listing: ask again shortly. */
  refreshing: boolean;
}

export interface ProviderInstance {
  external_id: string;
  name: string;
  gpu_model: string;
  gpu_count: number;
  power: string;
  price_per_hr: number;
  region: string;
  public_ip: string;
  ssh_port: number;
  ssh_user: string;
  ssh_connections: SSHConnection[];
  kind: string;
  node_id: number;
  /** What the instance allows now: power_on | power_off | install | delete. */
  actions: string[];
}

export interface SSHConnection {
  id: string;
  type: "direct" | "proxy" | "cloud_assistant" | string;
  label: LocalizedText;
  host: string;
  port: number;
  user: string;
  install_supported: boolean;
}

export interface ProviderInstallRequest {
  external_id: string;
  server_url?: string;
  ssh_key_pem?: string;
  password?: string;
  ssh_connection_id?: string;
  connect_mode?: "agent_dial" | "server_dial";
  server_dial_listen?: string;
  host_address?: string;
  server_dial_address?: string;
  // The operator agreed that opening a port for the agent restarts the
  // instance (RunPod answers INSTANCE_RESTART_REQUIRED until then).
  allow_instance_restart?: boolean;
  // The agent's install directory on the instance, absolute; omitted for
  // /opt/gpuworks.
  install_dir?: string;
  // Run the agent under nohup instead of the gpuworks-agent systemd unit.
  no_systemd?: boolean;
}

export interface ProviderInstallResponse {
  node_uuid?: string;
  status: string;
  connect_mode: "agent_dial" | "server_dial";
  server_dial_address?: string;
  server_url?: string;
  ssh_connection?: SSHConnection;
  ssh_attempts?: SSHAttempt[];
  checks?: CheckResult[];
  // The steps behind the summary checks: how the platform key reached the
  // instance, which SSH credential worked, and the driver's own checks.
  install_details?: CheckResult[];
  // Every item behind the one-line "precheck" row, passing ones included.
  precheck_details?: CheckResult[];
}

export interface SSHAttempt {
  connection_id: string;
  target: string;
  status: "selected" | "failed";
  detail?: string;
}
