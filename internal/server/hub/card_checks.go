package hub

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// SetCardCheckSinks wires each status report's cards, as they arrive, and
// the end of a node's session to whoever keeps the card checks in memory
// (cardhealth.Tracker); unset, neither goes anywhere.
func (h *Hub) SetCardCheckSinks(observe func(nodeUUID string, nodeID uint, at time.Time, gpus []proto.GPUInfo), gone func(nodeUUID string)) {
	h.onCardChecks, h.onNodeGone = observe, gone
}
