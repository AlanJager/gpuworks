//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (e *materialEligibility) readinessEpoch() uint64 {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch
}

func (e *materialEligibility) readinessEpochMatches(epoch uint64) bool {
	ready, _ := e.readinessEpochMatchesResult(epoch)
	return ready
}

func (e *materialEligibility) readinessEpochMatchesResult(epoch uint64) (bool, string) {
	if e == nil {
		return false, "proof_missing"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.epoch != epoch {
		return false, "epoch_changed"
	}
	if e.stopped {
		return false, "stopped"
	}
	if e.runCtx != nil && e.runCtx.Err() != nil {
		return false, "run_canceled"
	}
	return true, ""
}

// Readiness may observe subsequent results from an already-running background
// Run after its captured successful renewal still lacks publication headroom.
// One cumulative cutoff retains the original captured-proof scheduling budget.
// This never starts/cancels a proof, repeats SQL, or retries a membership grant.
func (e *materialEligibility) waitForReadinessRenewal(ctx context.Context, maxWait, headroom time.Duration, epoch uint64, current func() bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return nil
	}
	e.mu.Lock()
	captured := e.proofCtx
	original := e.deadline
	now := e.now()
	waitFor := e.interval
	if remaining := original.Sub(now); remaining < waitFor {
		waitFor = remaining
	}
	if maxWait > 0 && maxWait < waitFor {
		waitFor = maxWait
	}
	wait := e.epoch == epoch && !e.stopped && !original.IsZero() &&
		(e.runCtx == nil || e.runCtx.Err() == nil) && captured != nil &&
		captured.Err() == nil && waitFor > 0 && now.Before(original)
	e.mu.Unlock()
	if !wait {
		return nil
	}
	cutoff := time.Now().Add(waitFor)
	timer := time.NewTimer(time.Until(cutoff))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil // Preserve the existing captured-proof cutoff behavior.
	case <-captured.Done():
	case <-e.proofSlot:
		e.proofSlot <- struct{}{}
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == nil || !current() {
			return errors.New("readiness business generation became unavailable")
		}
		e.mu.Lock()
		now = e.now()
		valid := e.epoch == epoch && !e.stopped && !e.deadline.IsZero() &&
			(e.runCtx == nil || e.runCtx.Err() == nil) && now.Before(e.deadline) &&
			(e.proofCtx == nil || e.proofCtx.Err() == nil)
		renewed := e.proofCtx != captured && e.deadline.After(original)
		enough := !now.Add(headroom).After(e.deadline)
		background := e.running && e.runCtx != nil && e.runCtx.Err() == nil && e.onInvalidated != nil
		e.mu.Unlock()
		if !valid {
			return errors.New("readiness proof changed or became unavailable")
		}
		if !time.Now().Before(cutoff) {
			// Seven seconds is a scheduling preference. At the original cutoff,
			// a current successful result may attempt the one existing final
			// grant with the minimum reserve; publication still checks five
			// seconds after that grant. No error or canceled epoch is rescued.
			if renewed && background && current() && ctx.Err() == nil &&
				e.readinessEpochMatches(epoch) && e.EligibleFor(store.ClusterMembershipGrantTimeout) && current() && ctx.Err() == nil {
				return nil
			}
			return errors.New("readiness renewal scheduling allowance exhausted")
		}
		// The original helper does not follow explicit standalone Reprove calls.
		// A failed result can be followed by success at the same eligibility epoch.
		// Production invalidation must retire the captured business generation
		// before Reprove releases its slot; current() enforces that fence.
		// Standalone calls or an uninstalled invalidation owner never follow.
		if !renewed || enough || !background {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			// Recheck the same captured generation, context and current result
			// before the terminal minimum-reserve decision above.
		case <-ticker.C:
		}
	}
}
