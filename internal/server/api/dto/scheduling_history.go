package dto

import "time"

// SchedulingHistoryEntry is one scheduling event of the cluster-wide history
// (GET /scheduling/history): when, on which deployment, what happened and
// why. Category is auto (the platform followed its settings), failure (the
// platform reacted to something that went wrong) or manual (an operator
// ordered it). Params are the event code's params as the deployment timeline
// carries them; Actor is the user who ordered a manual one, when known.
// DeploymentDeleted is set when the deployment no longer exists: its name is
// kept from what it left behind.
type SchedulingHistoryEntry struct {
	ID                uint           `json:"id"`
	CreatedAt         time.Time      `json:"created_at"`
	DeploymentID      uint           `json:"deployment_id"`
	Deployment        string         `json:"deployment,omitempty"`
	DeploymentDeleted bool           `json:"deployment_deleted,omitempty"`
	Instance          string         `json:"instance,omitempty"`
	Kind              string         `json:"kind"`
	Code              string         `json:"code"`
	Category          string         `json:"category"`
	Params            map[string]any `json:"params"`
	Actor             string         `json:"actor,omitempty"`
	// Target is the deployment the entry acted for (params.for_deployment:
	// the one a yield gave the GPUs to), named; absent when there is none.
	Target *SchedulingHistoryDeployment `json:"target,omitempty"`
	// Nodes are the node or nodes the entry concerns; absent when the event
	// names none and the instances it names are gone.
	Nodes []SchedulingHistoryNode `json:"nodes,omitempty"`
	// Logs is where the output that explains a failed attempt can be read:
	// the instance and the window before its failure, for an entry about an
	// attempt that failed (a replica rebuilt after it, or retries given up).
	// Absent for other entries.
	Logs *SchedulingHistoryLogs `json:"logs,omitempty"`
}

// SchedulingHistoryLogs is a window of one instance's stored output, as the
// logs page takes it: Unix milliseconds.
type SchedulingHistoryLogs struct {
	Instance string `json:"instance"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
}

// SchedulingHistoryNode names a node an entry concerns. UUID is set only for
// a node the server still knows, so the UI can open it; Name is its display
// name, or what the event kept of a node that is gone.
type SchedulingHistoryNode struct {
	UUID string `json:"uuid,omitempty"`
	Name string `json:"name"`
}

// SchedulingHistoryDeployment names a deployment an entry refers to; Name is
// empty when it is unknown, Deleted when it is gone.
type SchedulingHistoryDeployment struct {
	ID      uint   `json:"id"`
	Name    string `json:"name,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}
