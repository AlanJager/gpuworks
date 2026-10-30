package dto

// DeploymentDGPUQuotaRequest is the body of PUT /deployments/{id}/dgpu: the
// dGPU memory limit each replica is to run under per card, from now on.
type DeploymentDGPUQuotaRequest struct {
	VRAMLimitGiB float64 `json:"vram_limit_gib"`
}

// DGPUQuotaResult answers a dGPU quota change: the deployment under its new
// limit, the instances held to it in place (Live, by UUID), and the ones that
// keep their own limit until their next restart (Pending).
type DGPUQuotaResult struct {
	Deployment Deployment         `json:"deployment"`
	Live       []string           `json:"live"`
	Pending    []DGPUQuotaPending `json:"pending"`
}

// DGPUQuotaPending is an instance a quota change leaves at its limit until
// it restarts. Reason is backend (its partition backend reads the limit only
// at start), asleep, starting, in_use (it holds UsedBytes, more than the new
// limit) or no_usage (what it holds is not known); the last four only for a
// lower limit.
type DGPUQuotaPending struct {
	InstanceUUID string `json:"instance_uuid"`
	Reason       string `json:"reason"`
	UsedBytes    int64  `json:"used_bytes,omitempty"`
}
