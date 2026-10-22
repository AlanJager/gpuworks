package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

// imageOperations tracks the image work this node is running, so one attempt
// can be stopped without touching another.
//
// A reference alone is not enough to identify a transfer: an operator whose
// pull is stuck can retry the same image, and for a while both attempts exist —
// one wedged inside its runtime CLI, one queued behind it. The transfer id is
// what the server sends, so cancelling names exactly one of them.
type imageOperations struct {
	mu  sync.Mutex
	ops map[string]context.CancelFunc
}

// operationKey prefers the transfer id and falls back to the reference, which
// is what an older server (one that sends no transfer id) can be cancelled by.
func operationKey(canonicalRef, transferID string) string {
	if id := strings.TrimSpace(transferID); id != "" {
		return "transfer:" + id
	}
	return "ref:" + strings.TrimSpace(canonicalRef)
}

// begin registers one operation and returns the function that unregisters it.
// The caller defers the returned func right after registering.
func (n *NodeRuntime) beginImageOperation(canonicalRef, transferID string, cancel context.CancelFunc) func() {
	key := operationKey(canonicalRef, transferID)
	n.imageOps.mu.Lock()
	if n.imageOps.ops == nil {
		n.imageOps.ops = map[string]context.CancelFunc{}
	}
	n.imageOps.ops[key] = cancel
	n.imageOps.mu.Unlock()
	return func() {
		n.imageOps.mu.Lock()
		delete(n.imageOps.ops, key)
		n.imageOps.mu.Unlock()
	}
}

// cancelImageOperation stops one registered operation. It reports whether
// anything was still running: cancelling an operation that already finished is
// a no-op, not an error, because the server may race a cancel against the
// terminal status.
func (n *NodeRuntime) cancelImageOperation(canonicalRef, transferID string) bool {
	key := operationKey(canonicalRef, transferID)
	n.imageOps.mu.Lock()
	cancel, ok := n.imageOps.ops[key]
	n.imageOps.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// rpcImageCancel stops one image operation. The work itself reports the
// terminal state (failed, with the cancellation as the reason), so this only
// has to name the right operation and acknowledge.
func (n *NodeRuntime) rpcImageCancel(_ context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageCancelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.CanonicalRef) == "" && strings.TrimSpace(p.TransferID) == "" {
		return nil, errors.New("image cancel requires a canonical_ref or a transfer_id")
	}
	state := "not_running"
	if n.cancelImageOperation(p.CanonicalRef, p.TransferID) {
		state = "cancelling"
	}
	return proto.ImageCancelResult{State: state, Cancelled: state == "cancelling"}, nil
}
