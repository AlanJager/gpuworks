package store

import (
	"encoding/json"

	"gorm.io/gorm"
)

// The residency timeline (residency_timeline.go) says which deployment held a
// card, and in what state, over a past window. An instance's row is deleted
// when its replica is replaced or removed, and with it the node and cards it
// held; the events it wrote carry neither. So removing an instance writes one
// more event, the last of its history, that records where it sat and when it
// left. Nothing else is stored for the timeline: the rest is read from the
// instance rows and the state and sleep events that already exist.

const (
	// EventKindResidency is the timeline kind of the residency timeline's own
	// events. It is a kind of its own so that finding the instances removed
	// lately is a range read of the (kind, id) index.
	EventKindResidency = "residency"
	// EventInstanceRemoved: the instance's row was deleted. Params: node_id,
	// gpus (the card indexes it held, as of the last probe). The event's time
	// is when it left the cards.
	EventInstanceRemoved = instanceEventRemoved
)

// recordInstanceRemoved appends the INSTANCE_REMOVED event for an instance
// about to be deleted, in the deleting transaction. An instance that never
// held a card has nothing to record.
func recordInstanceRemoved(tx *gorm.DB, inst *Instance) error {
	gpus := instanceGPUIndexes(inst)
	if inst.NodeID == 0 || len(gpus) == 0 {
		return nil
	}
	return addEventTo(tx, &InstanceEvent{InstanceID: inst.ID, InstanceUUID: inst.UUID, DeploymentID: inst.DeploymentID},
		EventKindResidency, EventInstanceRemoved, map[string]any{"node_id": inst.NodeID, "gpus": gpus})
}

// instanceGPUIndexes decodes the GPUIndexes column; an unreadable value holds
// no cards.
func instanceGPUIndexes(inst *Instance) []int {
	if inst == nil || inst.GPUIndexes == "" {
		return nil
	}
	var out []int
	if err := json.Unmarshal([]byte(inst.GPUIndexes), &out); err != nil {
		return nil
	}
	return out
}
