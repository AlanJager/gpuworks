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
func (o *imageOperations) begin(canonicalRef, transferID string, cancel context.CancelFunc) func() {
	key := operationKey(canonicalRef, transferID)
	o.mu.Lock()
	if o.ops == nil {
		o.ops = map[string]context.CancelFunc{}
	}
	o.ops[key] = cancel
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		delete(o.ops, key)
		o.mu.Unlock()
	}
}

// cancel stops one registered operation. It reports whether
// anything was still running: cancelling an operation that already finished is
// a no-op, not an error, because the server may race a cancel against the
// terminal status.
func (o *imageOperations) cancel(canonicalRef, transferID string) bool {
	key := operationKey(canonicalRef, transferID)
	o.mu.Lock()
	cancel, ok := o.ops[key]
	o.mu.Unlock()
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
	if n.imageOps.cancel(p.CanonicalRef, p.TransferID) {
		state = "cancelling"
	}
	return proto.ImageCancelResult{State: state, Cancelled: state == "cancelling"}, nil
}
