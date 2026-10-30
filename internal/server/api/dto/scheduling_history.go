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
}
