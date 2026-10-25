package app

import (
	"context"
	"encoding/json"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// stopModelCopy confirms worker exit without removing resumable bytes. The
// canceled intent stays fenced until the server supplies a new TransferID.
func (n *NodeRuntime) stopModelCopy(ctx context.Context, raw json.RawMessage) (any, error) {
	var request proto.ModelCopyStopParams
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if request.Key == "" || request.TransferID == "" {
		return nil, errors.New("copy key and transfer ID are required")
	}
	n.dlMu.Lock()
	active := n.downloading[request.Key]
	if active != nil && (!active.copy || active.transferID != request.TransferID) {
		n.dlMu.Unlock()
		return nil, errors.New("another model transfer owns this task")
	}
	if n.stoppedCopies == nil {
		n.stoppedCopies = map[string]string{}
	}
	n.stoppedCopies[request.Key] = request.TransferID
	if active != nil {
		active.cancel()
	}
	n.dlMu.Unlock()
	n.dropParkedTransfer(request.Key, request.TransferID)
	if active != nil {
		select {
		case <-active.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	n.progress.discard(modelFileProgressKey(request.Key))
	return proto.ModelCopyStopResult{Stopped: true}, nil
}
