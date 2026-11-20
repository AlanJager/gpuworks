//go:build linux && dqlite

package app

import (
	"context"
	"time"
)

func (e *materialEligibility) waitForWorkerProof(ctx context.Context, maxWait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return nil
	}
	e.mu.Lock()
	proofCtx := e.proofCtx
	waitFor := e.interval
	if maxWait > 0 && maxWait < waitFor {
		waitFor = maxWait
	}
	// Scheduling may yield to the first proof or an expired-result renewal.
	// It never reads or publishes the old result as business authority.
	wait := !e.stopped && (e.runCtx == nil || e.runCtx.Err() == nil) &&
		proofCtx != nil && proofCtx.Err() == nil && waitFor > 0
	e.mu.Unlock()
	if wait {
		timer := time.NewTimer(waitFor)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-proofCtx.Done():
		case <-timer.C:
		case <-e.proofSlot:
			e.proofSlot <- struct{}{}
		}
	}
	return ctx.Err()
}
