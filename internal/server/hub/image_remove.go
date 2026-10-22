package hub

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RemoveImageFromNode deletes one image from one node and marks the ledger row
// absent.
//
// The node is asked first: only it knows whether the runtime is willing to let
// the image go (a container may still be using it). The row is only rewritten
// once the deletion actually happened, so a refusal leaves the row saying what
// is still true — the image is there.
func (h *Hub) RemoveImageFromNode(ctx context.Context, imageID uint, targetNodeUUID string) (*store.ImageNode, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return nil, err
	}
	target, err := h.store.NodeByUUID(targetNodeUUID)
	if err != nil {
		return nil, err
	}
	if !h.Online(target.UUID) {
		return nil, errors.New("target node is offline")
	}
	if _, err := h.CallNode(ctx, target.UUID, proto.RPCImageRemove, proto.ImageRemoveParams{ImageRef: image.ImageRef}); err != nil {
		return nil, err
	}
	if err := h.store.MarkImageNodeAbsent(image.ID, target.ID, "image deleted from this node"); err != nil {
		return nil, err
	}
	return h.store.ImageNode(image.ID, target.ID)
}
