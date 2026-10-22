package hub

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CancelImageTransfer stops a node-side image transfer.
//
// The agent ends the runtime call it is running (the docker CLI is killed
// through its context), and the row is failed here as well: the node may have
// finished already, or lost the operation with its last report, and in every
// case the operator who asked for a stop has to end up with a row that can be
// retried. Cancelling is therefore both an RPC and a state change, best effort
// on the first and definite on the second.
func (h *Hub) CancelImageTransfer(ctx context.Context, imageID uint, targetNodeUUID string) (*store.ImageNode, error) {
	image, err := h.store.GetImage(imageID)
	if err != nil {
		return nil, err
	}
	target, err := h.store.NodeByUUID(targetNodeUUID)
	if err != nil {
		return nil, err
	}
	row, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil {
		return nil, err
	}
	return h.cancelImageTransferRow(ctx, image, target, row, "the transfer was cancelled")
}

// cancelImageTransferRow is the shared half: stop the runtime call on the node,
// then fail the row with the caller's reason.
func (h *Hub) cancelImageTransferRow(ctx context.Context, image *store.Image, target *store.Node, row *store.ImageNode, message string) (*store.ImageNode, error) {
	if row.State != store.ImageStatePulling && row.State != store.ImageStateSyncing {
		return row, nil // nothing is running; there is nothing to stop
	}
	if h.Online(target.UUID) {
		// The id names the attempt: the same image can have a stuck transfer and
		// a queued retry at the same time.
		method := proto.RPCImageCancel
		params := any(proto.ImageCancelParams{CanonicalRef: image.ImageRef, TransferID: row.TransferID})
		if row.Source == "peer" {
			// A peer transfer is owned by the direct receiver, not by the
			// runtime image operation registry. Stop that receiver explicitly so
			// its Receive context (and the docker load it is running) is cancelled.
			method = proto.RPCImageSyncStop
			params = proto.ImageSyncStopParams{TransferID: row.TransferID}
		}
		_, callErr := h.CallNode(ctx, target.UUID, method, params)
		if callErr != nil {
			h.log.Warn("image cancel rpc failed; failing the row anyway",
				"image", image.ImageRef, "node", target.UUID, "err", callErr)
		}
	}
	if _, failErr := h.store.CancelImageNodeTransfer(image.ID, target.ID, message); failErr != nil {
		return nil, failErr
	}
	return h.store.ImageNode(image.ID, target.ID)
}
