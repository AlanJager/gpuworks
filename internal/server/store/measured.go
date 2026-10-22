package store

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// measuredAfterReport decides what the measured column holds after a status
// report.
//
// The agent re-collects the breakdown from the container's log tail on every
// adopt/resync re-probe, but the engine prints that summary exactly once, at
// startup; once enough periodic log lines have followed it, a re-parse sees only
// a suffix of the summary and finds fewer fields. A re-parse of the same
// container must therefore never erase a field an earlier parse captured:
// fields the incoming parse found win, the rest keep their stored value. A
// report from a different container is a new generation — its parse replaces
// the column outright, since the stored values describe a different startup.
func measuredAfterReport(inst *Instance, st proto.InstanceStatus) string {
	incoming := string(st.Measured)
	if inst.Measured == "" || st.ContainerID == "" || st.ContainerID != inst.ContainerID {
		return incoming
	}
	var stored, fresh enginelog.Measured
	if json.Unmarshal([]byte(inst.Measured), &stored) != nil || json.Unmarshal([]byte(incoming), &fresh) != nil {
		return incoming
	}
	// Zero is the parser's own "not found" marker, so a zero in the incoming
	// parse means the line was outside the read window, not that the engine
	// reported zero memory.
	if fresh.ModelWeightsGiB != 0 {
		stored.ModelWeightsGiB = fresh.ModelWeightsGiB
	}
	if fresh.KVCacheGiB != 0 {
		stored.KVCacheGiB = fresh.KVCacheGiB
	}
	if fresh.KVCacheTokens != 0 {
		stored.KVCacheTokens = fresh.KVCacheTokens
	}
	if fresh.GraphCaptureGiB != 0 {
		stored.GraphCaptureGiB = fresh.GraphCaptureGiB
	}
	if fresh.MaxConcurrency != 0 {
		stored.MaxConcurrency = fresh.MaxConcurrency
	}
	if fresh.NumGPUBlocks != 0 {
		stored.NumGPUBlocks = fresh.NumGPUBlocks
	}
	stored.Found = stored.Found || fresh.Found
	merged, err := json.Marshal(stored)
	if err != nil {
		return incoming
	}
	return string(merged)
}
