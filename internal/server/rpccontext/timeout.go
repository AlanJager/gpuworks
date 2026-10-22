// Package rpccontext carries per-call RPC wait budgets through existing contexts.
package rpccontext

import (
	"context"
	"time"
)

type timeoutKey struct{}

// WithTimeout sets an explicit RPC wait budget and bounds the caller context.
// A shorter parent deadline or cancellation still takes precedence.
func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	return context.WithValue(ctx, timeoutKey{}, timeout), cancel
}

// Timeout returns the explicit RPC budget, or fallback for ordinary calls.
func Timeout(ctx context.Context, fallback time.Duration) time.Duration {
	if timeout, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return timeout
	}
	return fallback
}
