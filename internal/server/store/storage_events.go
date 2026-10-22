package store

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// Deployment timeline events for the shared storage a deployment's instances
// read their weights from (instance_model_files). A running instance keeps serving from GPU memory while the
// storage is unreachable, and one that is loading weights simply waits, so
// without these the timeline would only say "waiting for engine health".
const (
	EventStorageUnavailable = "STORAGE_UNAVAILABLE"
	EventStorageRecovered   = "STORAGE_RECOVERED"
)

// storageTransition is a node's shared storage turning unusable or usable.
type storageTransition struct {
	storage Storage
	ready   bool
	state   StorageNodeState
}

// readyChanged reports whether a node state moves into or out of ready. A
// first report that is ready is not a change; a first report that is not
// ready is.
func readyChanged(prev *StorageNodeState, next StorageNodeState) bool {
	wasReady := prev == nil || prev.State == proto.StorageStateReady
	return wasReady != (next.State == proto.StorageStateReady)
}

// recordStorageTransitions adds a timeline event to every deployment that
// reads from the storage and has a live instance on the node.
func (s *Store) recordStorageTransitions(nodeID uint, transitions []storageTransition) {
	if len(transitions) == 0 {
		return
	}
	nodeName := ""
	if node, err := s.NodeByID(nodeID); err == nil {
		nodeName = node.Name
		if nodeName == "" {
			nodeName = node.Hostname
		}
	}
	for _, tr := range transitions {
		depIDs, _, err := liveReadersOfStorage(s.db, tr.storage.ID, nodeID)
		if err != nil {
			continue
		}
		code := EventStorageUnavailable
		params := map[string]any{"storage": tr.storage.Name, "node": nodeName}
		if tr.ready {
			code = EventStorageRecovered
		} else {
			params["state"], params["reason"] = tr.state.State, tr.state.Message
		}
		for _, id := range depIDs {
			s.AddDeploymentEventCode(id, EventKindSchedule, code, params)
		}
	}
}
