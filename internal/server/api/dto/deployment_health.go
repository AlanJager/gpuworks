package dto

// DeploymentHealth is a deployment's health check (internal/server/healthcheck):
// whether it is on, how its latest check went and the recent ones. It only
// reports; nothing acts on it.
type DeploymentHealth struct {
	// Status is ok (answered within the threshold), timeout, failed (an
	// error reply, or no replica reachable), off (the check is not on),
	// no_replica (nothing running awake to check) or pending (on, not
	// checked yet).
	Status string `json:"status"`
	// ThresholdSec is how long a check waits for its reply.
	ThresholdSec int `json:"threshold_sec"`
	// Last is the latest check that ran; null before the first.
	Last *HealthProbe `json:"last"`
	// RecentMeanMS is the mean time of the recent checks answered within
	// the threshold, RecentOK how many there were; null with none.
	RecentMeanMS *int64 `json:"recent_mean_ms"`
	RecentOK     int    `json:"recent_ok"`
	// Recent are the last checks, up to 10, oldest first.
	Recent []HealthProbe `json:"recent"`
}

// HealthProbe is one health check.
type HealthProbe struct {
	AtMS int64 `json:"at_ms"`
	// Status is ok, timeout or failed.
	Status string `json:"status"`
	// LatencyMS is how long the reply took; a timeout's is the threshold.
	LatencyMS int64 `json:"latency_ms"`
	// HTTPStatus is the reply's status; absent when none came.
	HTTPStatus int `json:"http_status,omitempty"`
	// InstanceUUID is the replica that answered.
	InstanceUUID string `json:"instance_uuid,omitempty"`
	// Message is the start of an error reply, or why none came.
	Message string `json:"message,omitempty"`
}

// HealthCheckRequest is PUT /deployments/{id}/health-check.
type HealthCheckRequest struct {
	Enabled bool `json:"enabled"`
	// ThresholdSec is how long a check waits, 1–600; 0 keeps the current.
	ThresholdSec int `json:"threshold_sec,omitempty"`
}
