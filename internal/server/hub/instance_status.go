package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
)

// applyInstanceStatus keeps the current-session check ordered with the store
// write. register needs the exclusive side of the same mutex, so it cannot
// replace this session after validation but before application. The node-bound
// store method then rejects a valid current session reporting another node's
// instance.
func (h *Hub) applyInstanceStatus(session *Session, status proto.InstanceStatus) (bool, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if session == nil || h.sessions[session.NodeUUID] != session {
		return false, nil
	}
	node, err := h.store.NodeByUUID(session.NodeUUID)
	if err != nil {
		return false, err
	}
	if err := h.store.UpdateInstanceStatusFromNode(node.ID, status); err != nil {
		return false, err
	}
	return true, nil
}
