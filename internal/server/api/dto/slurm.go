package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// SlurmCluster is a Slurm cluster whose GPU nodes join through a health check
// the administrator puts in slurm.conf. The three commands carry the
// enrollment token and are present for administrators only.
type SlurmCluster struct {
	ID        uint   `json:"id"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	ServerURL string `json:"server_url"`
	// SlurmClusterName is Slurm's own ClusterName, bound by the first node that
	// enrolls; empty until then.
	SlurmClusterName string `json:"slurm_cluster_name"`
	// RunAs is the account the agents run as; AccountUser the account their
	// placeholder jobs are submitted as (empty: the agent's own).
	RunAs       string `json:"run_as"`
	AccountUser string `json:"account_user"`
	// InstallDir is each agent's install directory and ScriptDir the
	// directory of the health-check program, the same path on every node.
	InstallDir string `json:"install_dir"`
	ScriptDir  string `json:"script_dir"`
	// SbatchArgs, IdleReturnSeconds and PendingTimeoutSeconds are the settings
	// every agent of the entry applies to its placeholder jobs; the agents pick
	// an edit up on their next report (GET /slurm/defaults, report_interval_seconds).
	SbatchArgs            string `json:"sbatch_args"`
	IdleReturnSeconds     int    `json:"idle_return_seconds"`
	PendingTimeoutSeconds int    `json:"pending_timeout_seconds"`
	Revoked               bool   `json:"revoked"`
	// State is where the entry stands, the one value a client shows and
	// filters by: revoked, stalled (no node joined for too long), pending (no
	// node yet) or active (a node joined). Stalled is the same fact on its own.
	State   string `json:"state"`
	Stalled bool   `json:"stalled"`
	// NodeCount is the nodes that enrolled; SlurmGPUNodes the GPU nodes Slurm
	// lists, as an enrolled agent last reported them (0 before the first
	// report), plus the nodes that enrolled within a report interval and a
	// minute and are not on the list yet: every node that enrolled has a GPU,
	// and a report can be older than the enrollment. MissingCount is the listed nodes with no agent online, and
	// MissingNodes the first of them, by name (at most
	// GET /slurm/defaults missing_nodes_limit).
	NodeCount int `json:"node_count"`
	// OnlineCount is the enrolled nodes whose agent is online: what the
	// console counts as joined. The others are offline (NodeCount - OnlineCount).
	OnlineCount   int `json:"online_count"`
	SlurmGPUNodes int `json:"slurm_gpu_nodes"`
	// ListAsOf is set while no agent of the entry is online and its list of
	// GPU nodes is not empty: only its agents report the list, so it is the
	// one from before they went, at most as new as when an agent was last
	// seen, which this is. SlurmGPUNodes then counts that list.
	ListAsOf     *time.Time         `json:"list_as_of,omitempty"`
	MissingCount int                `json:"missing_count"`
	MissingNodes []SlurmMissingNode `json:"missing_nodes"`
	// Waiting says why no node has enrolled yet, while none has and the entry
	// is not revoked, when the server can tell; absent otherwise (a node
	// enrolled, or nothing known is the cause).
	Waiting *SlurmWaiting `json:"waiting,omitempty"`
	// SharedStorageHint suggests registering the cluster's shared filesystem
	// as model storage, with what the console fills the add-storage form
	// with; set for an entry made from an E-HPC cluster that has joined nodes
	// none of which has a shared storage yet, absent otherwise.
	SharedStorageHint *SlurmSharedStorageHint `json:"shared_storage_hint,omitempty"`
	// Category is the platform type the entry is listed under when it was
	// connected on its own, and Resources what it connects (the slurm entry of
	// GET /providers/kinds categories).
	Category  string             `json:"category"`
	Resources []PlatformResource `json:"resources"`
	// Source is the cloud account cluster the entry was made from, if any.
	Source    *SlurmClusterSource `json:"source,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	// CanApply says the platform can run the setup on the cluster's controller
	// itself (POST /slurm/clusters/{uuid}/apply): the entry was made from a
	// cloud account's Slurm cluster whose driver can run a command on the
	// manager node, and the entry is not revoked. CanRemove says it can take
	// the setup out again (POST .../remove), which a revoked entry still
	// allows. When it cannot, OperationBlocked is why, a code: no_source (the
	// entry was not made from a cloud account's cluster), no_provider (the
	// account is gone), provider_unsupported (its driver cannot run commands
	// on the manager node), cluster_unknown (no synced node and not in the
	// account's listing), not_slurm_cluster (the cluster schedules with
	// something else) or revoked (only applying is refused).
	CanApply         bool   `json:"can_apply"`
	CanRemove        bool   `json:"can_remove"`
	OperationBlocked string `json:"operation_blocked,omitempty"`
	// DeleteWithNodesAllowed says DELETE /slurm/clusters/{uuid}?with_offline_nodes=true
	// would succeed now; when it would not, DeleteWithNodesBlocker is why:
	// "online" (an agent of the entry is connected) or "instances" (a node of
	// the entry has instances). Both are meaningful while NodeCount > 0.
	DeleteWithNodesAllowed bool   `json:"delete_with_nodes_allowed"`
	DeleteWithNodesBlocker string `json:"delete_with_nodes_blocker,omitempty"`
	// SetupCommand installs the health check on the cluster's controller and is
	// absent once the entry is revoked; RemoveCommand takes it out again and
	// UninstallAgentsCommand is the first step of uninstalling the agents (see
	// GET /slurm/clusters/{uuid}/setup).
	SetupCommand string `json:"setup_command,omitempty"`
	// CheckCommand runs the same setup in check-only mode: it reports what
	// the setup would do and changes nothing. It is absent with SetupCommand.
	CheckCommand           string `json:"check_command,omitempty"`
	RemoveCommand          string `json:"remove_command,omitempty"`
	UninstallAgentsCommand string `json:"uninstall_agents_command,omitempty"`
}

// SlurmMissingNode is a GPU node Slurm lists that has no agent online. Reason
// is a code: not_enrolled (no agent of the entry ever enrolled there: the
// health check has not run on it, or it has no NVIDIA GPU the check finds) or
// offline (an agent enrolled and is not connected). SlurmState and SlurmReason
// are Slurm's own words for the node, as reported.
type SlurmMissingNode struct {
	Name        string `json:"name"`
	Reason      string `json:"reason"`
	SlurmState  string `json:"slurm_state"`
	SlurmReason string `json:"slurm_reason,omitempty"`
	// NodeUUID is the node's, for a node that enrolled and is offline: the
	// console offers to remove it (DELETE /nodes/{uuid}), for a node that
	// left the cluster while no agent was left to say so.
	NodeUUID string `json:"node_uuid,omitempty"`
}

// SlurmClusterSource names the cloud account cluster an entry was made from.
type SlurmClusterSource struct {
	ProviderID        uint   `json:"provider_id"`
	ProviderName      string `json:"provider_name"`
	ProviderKind      string `json:"provider_kind"`
	ProviderClusterID string `json:"provider_cluster_id"`
}

// SlurmDefaults is what the cluster form needs from the server: the values an
// omitted field takes and the bounds it enforces.
type SlurmDefaults struct {
	RunAs                      string `json:"run_as"`
	InstallDir                 string `json:"install_dir"`
	ScriptDir                  string `json:"script_dir"`
	IdleReturnSeconds          int    `json:"idle_return_seconds"`
	IdleReturnMinSeconds       int    `json:"idle_return_min_seconds"`
	IdleReturnMaxSeconds       int    `json:"idle_return_max_seconds"`
	PendingTimeoutSeconds      int    `json:"pending_timeout_seconds"`
	PendingTimeoutMinSeconds   int    `json:"pending_timeout_min_seconds"`
	PendingTimeoutMaxSeconds   int    `json:"pending_timeout_max_seconds"`
	HealthCheckIntervalSeconds int    `json:"health_check_interval_seconds"`
	StalledAfterSeconds        int    `json:"stalled_after_seconds"`
	ReportIntervalSeconds      int    `json:"report_interval_seconds"`
	MissingNodesLimit          int    `json:"missing_nodes_limit"`
	// QuickTunnelURL is the platform's own temporary public address while one
	// is on (`gpuworks expose`, a Cloudflare quick tunnel), "" when there is
	// none. A server URL that is this address must not go to the nodes: it
	// changes whenever the connector is rebuilt (docs/how-to/expose-public-address.md).
	QuickTunnelURL string `json:"quick_tunnel_url"`
}

// SlurmClusterRequest creates a cluster. Empty optional fields take the
// defaults GET /slurm/defaults shows.
type SlurmClusterRequest struct {
	Name        string `json:"name"`
	ServerURL   string `json:"server_url"`
	RunAs       string `json:"run_as,omitempty"`
	AccountUser string `json:"account_user,omitempty"`
	InstallDir  string `json:"install_dir,omitempty"`
	ScriptDir   string `json:"script_dir,omitempty"`
	SbatchArgs  string `json:"sbatch_args,omitempty"`
	// IdleReturnSeconds and PendingTimeoutSeconds are omitted for the defaults.
	IdleReturnSeconds     *int `json:"idle_return_seconds,omitempty"`
	PendingTimeoutSeconds *int `json:"pending_timeout_seconds,omitempty"`
	// ProviderID and ProviderClusterID name the cloud account's HPC cluster the
	// entry is made from; both or neither.
	ProviderID        *uint  `json:"provider_id,omitempty"`
	ProviderClusterID string `json:"provider_cluster_id,omitempty"`
}

// SlurmClusterUpdate edits a cluster's settings: every field is optional,
// omitted means unchanged, and an empty string clears (name excepted). The
// server URL, the agents' account and the two directories cannot be edited.
type SlurmClusterUpdate struct {
	Name                  *string `json:"name,omitempty"`
	AccountUser           *string `json:"account_user,omitempty"`
	SbatchArgs            *string `json:"sbatch_args,omitempty"`
	IdleReturnSeconds     *int    `json:"idle_return_seconds,omitempty"`
	PendingTimeoutSeconds *int    `json:"pending_timeout_seconds,omitempty"`
}

// SlurmClusterEdit is the answer to an edit: the entry as stored and the
// fields that changed (their JSON names, sorted). Agents apply an edit on
// their next report; nothing is re-run on the cluster.
type SlurmClusterEdit struct {
	Cluster SlurmCluster `json:"cluster"`
	Changed []string     `json:"changed"`
}

// SlurmSetup is what an administrator runs on the cluster, for the entry:
// the commands and the scripts' URLs (each carries the enrollment token).
// Every command runs as root on the Slurm controller. Uninstalling is two
// steps: UninstallAgentsCommand, wait one health-check interval
// (IntervalSeconds, what slurm.conf holds) while every agent uninstalls
// itself, then RemoveCommand.
type SlurmSetup struct {
	SetupCommand           string `json:"setup_command,omitempty"`
	CheckCommand           string `json:"check_command,omitempty"`
	RemoveCommand          string `json:"remove_command"`
	UninstallAgentsCommand string `json:"uninstall_agents_command"`
	SetupURL               string `json:"setup_url,omitempty"`
	CheckScriptURL         string `json:"check_script_url,omitempty"`
	// DefaultIntervalSeconds is the HealthCheckInterval setup writes when the
	// cluster has none of its own.
	DefaultIntervalSeconds int `json:"default_interval_seconds"`
}

// The agent-facing Slurm exchange (enrollment and the cluster's node report)
// is shared with the agent, which cannot import the server: its shapes are
// defined in proto, and these are those types.
type (
	SlurmEnrollRequest         = proto.SlurmEnrollRequest
	SlurmAgentSettings         = proto.SlurmAgentSettings
	SlurmEnrollResponse        = proto.SlurmEnrollResponse
	SlurmGPUNodeReport         = proto.SlurmGPUNodeReport
	SlurmGPUNodeEntry          = proto.SlurmGPUNodeEntry
	SlurmGPUNodeReportResponse = proto.SlurmGPUNodeReportResponse
)

// SlurmTokenStatus answers GET /slurm/token-status?token=: whether the
// enrollment token is "active", "revoked" or "unknown". It is what the health
// check asks before it installs an agent, and tells nothing else.
type SlurmTokenStatus struct {
	Status string `json:"status"`
}

// SlurmApplyOutcome is POST /slurm/clusters/{uuid}/apply finishing: what the
// platform did on the cluster's manager node, through the cloud account's
// command channel. Status is "ok" or "partial" (it finished, but something
// is left to do by hand: see the steps). A step that cannot finish is an
// error answer instead (SLURM_APPLY_*).
type SlurmApplyOutcome struct {
	Status          string               `json:"status"`
	ManagerInstance string               `json:"manager_instance"`
	Steps           []SlurmApplyStep     `json:"steps"`
	Gres            SlurmApplyGres       `json:"gres"`
	Distribute      SlurmApplyDistribute `json:"distribute"`
	// Resumed are the compute nodes that were resumed because Slurm still held
	// them for the missing GPUs (a drain an administrator set is left alone),
	// ResumeFailed those that could not be resumed.
	Resumed      []string `json:"resumed_nodes"`
	ResumeFailed []string `json:"resume_failed_nodes"`
	// Reach is whether the server's address could be reached from the
	// controller and from each GPU compute node.
	Reach   SlurmReach   `json:"reach"`
	Cluster SlurmCluster `json:"cluster"`
}

// SlurmReach says, host by host, whether the entry's server URL could be
// reached (its /healthz): from the controller, and from each GPU compute node
// the platform could ask. A node that could not be asked is Status "unknown"
// with why; it is no failure. NodesUnlisted is why the cloud's compute nodes
// could not be listed to ask them ("" when they were): permission_missing (with
// NodesPermission, the action) or failed.
type SlurmReach struct {
	Hosts           []SlurmReachHost `json:"hosts"`
	NodesUnlisted   string           `json:"nodes_unlisted,omitempty"`
	NodesPermission string           `json:"nodes_permission,omitempty"`
}

// SlurmReachHost is one host's answer. Role is controller or node; Status ok,
// failed or unknown; Reason, for failed, dns, connect, timeout, tls, http,
// no_curl or other, and for unknown not_asked, node_not_running,
// assistant_offline or command_failed.
type SlurmReachHost struct {
	Role   string `json:"role"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// SlurmWaiting is the server's reading of why an entry has no node. Reason is
// a code: no_compute_nodes (the cloud lists no compute nodes in the cluster),
// server_unreachable (the last check or apply found a host that cannot reach
// the server's address: Hosts are those, with why), not_applied (the setup
// was not seen run on the cluster) or no_node_called (it was, and no node has
// asked the server since). What the server has not seen since it started is
// not claimed.
type SlurmWaiting struct {
	Reason string           `json:"reason"`
	Hosts  []SlurmReachHost `json:"hosts,omitempty"`
}

// SlurmApplyStep is one step of an apply, in the order they run. ID is
// gres, check_script, distribute, slurm_conf, reconfigure, resume or reach
// (the server's address could be reached; absent when no node was asked); Status is
// pass, warn (done, needs attention), skip (nothing to do) or fail.
type SlurmApplyStep struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// SlurmApplyGres is what became of gres.conf: Added are the compute nodes
// Slurm gives a gpu Gres that got a line, Present the nodes that already had
// one (never changed), and NoGPUGres the nodes the account lists with a card
// whose Gres lists no gpu in Slurm (nothing was added for them: a gres.conf
// line would make slurmd refuse them).
type SlurmApplyGres struct {
	Added     []string `json:"added"`
	Present   []string `json:"present"`
	NoGPUGres []string `json:"no_gpu_gres"`
}

// SlurmApplyDistribute is how the check script reached the compute nodes:
// Shared when the script directory is on shared storage (nothing is copied),
// otherwise OK nodes got it and Failed are the nodes it could not be copied to.
type SlurmApplyDistribute struct {
	Shared bool     `json:"shared"`
	OK     int      `json:"ok"`
	Failed []string `json:"failed"`
}

// SlurmRemoveRequest is the body of POST /slurm/clusters/{uuid}/remove: the
// uninstall step to run on the manager node. The steps go in order:
// uninstall_agents (every node's check removes its agent on its next run),
// then, after the wait the answer names, remove_check (slurm.conf gets the
// original health check back).
type SlurmRemoveRequest struct {
	Step string `json:"step"`
}

// SlurmRemoveOutcome is the answer to a remove step. WaitSeconds is how long
// to wait before the next step, NextStep the step to run then ("" after the
// last one).
type SlurmRemoveOutcome struct {
	Step            string       `json:"step"`
	Status          string       `json:"status"`
	ManagerInstance string       `json:"manager_instance"`
	WaitSeconds     int          `json:"wait_seconds"`
	NextStep        string       `json:"next_step,omitempty"`
	Cluster         SlurmCluster `json:"cluster"`
}

// ProviderHPCCluster is an HPC cluster of a cloud account (provider.HPCClusterListing,
// GET /providers/{id}/hpc-clusters), listed live whether or not a node of it
// was synced, and the entry made from it, if any.
type ProviderHPCCluster struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	// Scheduler is the cluster's workload manager, lower case ("slurm",
	// "pbs"...), and SchedulerVersion its version as the vendor reports it;
	// either is empty when the vendor did not say.
	Scheduler        string `json:"scheduler"`
	SchedulerVersion string `json:"scheduler_version"`
	// State is the cluster's state as a stable code (uninit, creating, initing,
	// pending, running, exception, stopping, stopped, releasing, or unknown
	// for a word the vendor does not document), StateTone the colour it is
	// shown in (success, info, neutral, danger) and VendorState the vendor's
	// own word. Running says the vendor reports it usable.
	State       string `json:"state"`
	StateTone   string `json:"state_tone"`
	VendorState string `json:"vendor_state"`
	Running     bool   `json:"running"`
	// ComputeNodes is how many compute nodes the cloud lists in the cluster
	// right now, in any state (creating, running, abnormal), from the same
	// live listing as the rest of the row; absent when the listing carries no
	// count (unknown, not zero). The cloud does not say which of them have a
	// GPU, so it is not a GPU node count.
	ComputeNodes *int `json:"compute_nodes,omitempty"`
	// GPUNodes is the cluster's compute nodes with a GPU as the account's last
	// instance sync listed them; absent until a sync has listed the cluster
	// (unknown, not zero), and absent whenever ComputeNodes is present: the
	// live count is preferred and the sync, which can be old, is only the
	// fallback.
	GPUNodes *int `json:"gpu_nodes,omitempty"`
	// Connectable: the cluster runs, schedules with Slurm and has no entry
	// yet, so an entry can be made from it. CanApply: the platform can also
	// run the setup on its manager node itself. UnsupportedReason says why a
	// cluster with no entry cannot be connected at all, "" when it can or has
	// one: "scheduler_not_slurm" (only Slurm's cards the agent shares).
	// NotConnectableReason says why a Slurm cluster with no entry cannot be
	// connected yet because of its state, "" when it can or has one:
	// cluster_starting, cluster_pending, cluster_exception, cluster_stopped,
	// cluster_releasing or cluster_state_unknown. At most one of the two
	// reasons is set; the scheduler's comes first as it does not pass.
	Connectable          bool   `json:"connectable"`
	CanApply             bool   `json:"can_apply"`
	UnsupportedReason    string `json:"unsupported_reason,omitempty"`
	NotConnectableReason string `json:"not_connectable_reason,omitempty"`
	// Entry is the Slurm entry made from the cluster.
	Entry *SlurmClusterRef `json:"entry,omitempty"`
}

// ProviderHPCClusterList is GET /providers/{id}/hpc-clusters: the clusters
// (items, total) and how far the cloud let the account be listed. Access is
// "ok", "permission_missing" (the key may not list E-HPC clusters: Permission
// is the RAM action, and items is empty) or "not_activated" (the account never
// activated E-HPC: Code is the cloud's word, items is empty); neither is an
// error, the cluster list is simply empty. Partial is true when the clusters
// of some regions could not be read, those regions being FailedRegions, so a
// cluster of theirs may be missing from items.
type ProviderHPCClusterList struct {
	Items         []ProviderHPCCluster      `json:"items"`
	Total         int                       `json:"total"`
	Access        string                    `json:"access"`
	Permission    string                    `json:"permission,omitempty"`
	Code          string                    `json:"code,omitempty"`
	Partial       bool                      `json:"partial"`
	FailedRegions []ProviderHPCFailedRegion `json:"failed_regions"`
	// ListedAt is when the cloud was read for this listing; Refreshing says
	// a newer read is going on, which the page asks for again shortly.
	ListedAt   *time.Time `json:"listed_at,omitempty"`
	Refreshing bool       `json:"refreshing"`
}

// ProviderHPCFailedRegion is a region whose E-HPC clusters could not be listed:
// Code and Message are the cloud's, Permission the RAM action the key lacks when
// that is why.
type ProviderHPCFailedRegion struct {
	Region     string `json:"region"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Permission string `json:"permission,omitempty"`
}

// SlurmClusterRef names a Slurm cluster entry.
type SlurmClusterRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// SlurmSharedStorageHint is the storage the console offers to add for a
// cluster: a mounted shared directory at Path, named Name, applied to the
// nodes labelled LabelKey=LabelValue (the entry's nodes).
type SlurmSharedStorageHint struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	LabelKey   string `json:"label_key"`
	LabelValue string `json:"label_value"`
}
