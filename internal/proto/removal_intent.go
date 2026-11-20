package proto

// InstanceRemovalIntent identifies one versioned removal. Version stays stable
// across retries. It is the body RemovalActionPayloadSHA256 binds.
type InstanceRemovalIntent struct {
	UUID    string `json:"uuid"`
	Version uint64 `json:"version"`
}

// ActionDrainInspectParams asks whether calls from an older scheduling
// generation are still executing.
type ActionDrainInspectParams struct {
	ThroughGeneration uint64 `json:"through_generation"`
}

// ActionDrainInspectResult is the agent's answer to ActionDrainInspectParams.
type ActionDrainInspectResult struct {
	NodeUUID          string `json:"node_uuid"`
	SessionGeneration uint64 `json:"session_generation"`
	SessionUUID       string `json:"session_uuid"`
	ConnectionID      string `json:"connection_id"`
	ThroughGeneration uint64 `json:"through_generation"`
	PendingCalls      int    `json:"pending_calls"`
	ObservedAtUnixMS  int64  `json:"observed_at_unix_ms"`
}
