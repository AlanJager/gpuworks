package proto

// The HTTP exchange between an agent a Slurm cluster entry's health check
// installed and the server (ADR 0075): enrollment (POST /api/v1/slurm/enroll)
// and the cluster's node report (POST /api/v1/slurm/gpu-nodes). The agent
// cannot import the server, so both sides take these shapes from here.

// SlurmReportIntervalSeconds is how often such an agent reports the cluster's
// nodes, and so the longest an edit of the entry's settings takes to reach
// it; the server also counts on it when it says how fresh the node list is.
const SlurmReportIntervalSeconds = 600

// SlurmAgentSettings are the entry settings an agent applies to its
// placeholder jobs: in the enrollment answer and in every report's answer.
// A non-positive duration means the agent's own default.
type SlurmAgentSettings struct {
	SbatchArgs            string `json:"sbatch_args"`
	IdleReturnSeconds     int    `json:"idle_return_seconds"`
	PendingTimeoutSeconds int    `json:"pending_timeout_seconds"`
	// AccountUser is the account placeholder jobs are submitted as when the
	// agent runs as root; empty submits as the agent's own account.
	AccountUser string `json:"account_user"`
}

// SlurmEnrollRequest trades the entry's enrollment token for this node's
// identity: the Slurm cluster's ClusterName and this node's Slurm name.
type SlurmEnrollRequest struct {
	Token            string `json:"token"`
	SlurmClusterName string `json:"slurm_cluster_name"`
	NodeName         string `json:"node_name"`
}

// SlurmEnrollResponse is the node the agent now is, and the entry's settings.
type SlurmEnrollResponse struct {
	NodeUUID  string             `json:"node_uuid"`
	NodeToken string             `json:"node_token"`
	Settings  SlurmAgentSettings `json:"settings"`
}

// SlurmGPUNodeReport is an agent's list of the cluster's nodes, from sinfo
// (every partition, hidden ones too). Digest identifies the list's membership
// (the node names, which of them have a GPU GRES and how many GPUs; opaque to
// the server, which only compares it with the one it stored), StateDigest the
// state and reason of the GPU nodes. An agent sends the two digests alone
// first (Nodes nil: null on the wire, absent from earlier agents; the server
// reads both as no list), and the nodes only when the server
// asks for them (NeedNodes): then a cluster with none sends an empty list,
// which is a list, not its absence.
type SlurmGPUNodeReport struct {
	Digest      string              `json:"digest"`
	StateDigest string              `json:"state_digest"`
	Nodes       []SlurmGPUNodeEntry `json:"nodes"`
}

// SlurmGPUNodeEntry is one node of a report. ListedOnly: the node has no gpu
// GRES; it is listed so that the platform knows Slurm still has it, and has no
// state, reason or GPUs here.
type SlurmGPUNodeEntry struct {
	Name       string `json:"name"`
	ListedOnly bool   `json:"listed_only,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	GPUs       int    `json:"gpus"`
}

// SlurmGPUNodeReportResponse answers a report: NeedNodes says the server wants
// the list the digests name (send it again with the nodes), Settings the
// entry's current agent settings.
type SlurmGPUNodeReportResponse struct {
	NeedNodes bool               `json:"need_nodes"`
	Settings  SlurmAgentSettings `json:"settings"`
}
