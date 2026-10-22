package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Session replacement and status application share the same ordering, as
// instance reports do. A delayed old connection cannot finish a new copy, nor
// recreate the copy observations its replacement dropped.
func (h *Hub) applyModelFileStatus(session *Session, status proto.ModelFileStatus) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if session == nil || h.sessions[session.NodeUUID] != session {
		return nil
	}
	// Recorded before the store publishes the progress event, so a reader
	// woken by that event sees this frame.
	h.copies.Record(session.nodeID, status, h.now())
	return h.store.UpdateModelFileStatus(session.nodeID, status)
}

// ModelCopyObservation is the latest progress frame of the copy the row now
// intends, if its node is connected and reported one recently.
func (h *Hub) ModelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool) {
	return h.copies.Observe(store.ModelFileKey(file.ID), file.State, file.TransferID, h.now())
}
