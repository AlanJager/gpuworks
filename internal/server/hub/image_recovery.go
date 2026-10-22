package hub

import "context"

// resumeDirectImageTransfers re-coordinates persisted syncing rows whenever a
// source or target reconnects. The new attempt uses a new token/transfer id;
// Docker archives are intentionally restarted from byte zero.
func (h *Hub) resumeDirectImageTransfers(nodeUUID string) {
	node, err := h.store.NodeByUUID(nodeUUID)
	if err != nil {
		return
	}
	rows, err := h.store.DirectImageTransfersForNode(node.ID)
	if err != nil {
		return
	}
	for _, row := range rows {
		target, targetErr := h.store.NodeByID(row.NodeID)
		source, sourceErr := h.store.NodeByID(row.SourceNodeID)
		if targetErr != nil || sourceErr != nil || !h.Online(target.UUID) || !h.Online(source.UUID) {
			continue
		}
		if target.UUID != nodeUUID && source.UUID != nodeUUID {
			continue
		}
		_, _ = h.syncImageDirect(context.Background(), row.ImageID, target.UUID, source.UUID, row.RateLimitKiB, true)
	}
}
