//go:build linux && dqlite

package app

import (
	"context"
	"time"
)

// businessTransientReopenWait bounds how long a request waits for a transient
// closure to reopen: a leadership flap or a rejoin closes the write face for
// roughly one reproof interval, and the loop's next successful proof restores
// it. It never extends a definitive closure.
const businessTransientReopenWait = 3 * time.Second

// activelyReproofing reports a live background proof loop whose next
// successful proof can reopen the current closure: a leadership flap or a
// rejoin closes admission for seconds while the loop keeps retrying. A
// stopped, canceled, or never-started loop is a definitive closure and must
// not hold requests. The invalidation owner is required, as in the
// background check in waitForReadinessRenewal. A closure is transient only
// within the lifetime the last successful proof would have had: a member
// that has not proven since then (a partitioned minority, or one that never
// proved) refuses at once instead of after every request's wait budget.
func (e *materialEligibility) activelyReproofing() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running && !e.stopped && e.runCtx != nil && e.runCtx.Err() == nil && e.onInvalidated != nil &&
		!e.provedAt.IsZero() && e.now().Before(e.provedAt.Add(materialEligibilityWindow))
}

// awaitTransientReopen holds a business request only while a live background
// proof loop can still reopen admission: a flap or rejoin closes the gate for
// seconds and the next successful proof reinstalls readiness. A stopped,
// canceled, or never-started loop is a definitive closure and returns at
// once. The budget is a hard cap and the request context cancels the wait
// immediately; the caller rechecks admission after it returns.
func (g *businessHandlerGate) awaitTransientReopen(ctx context.Context, budget time.Duration) {
	eligibility := g.materialEligibility.Load()
	if eligibility == nil || !eligibility.activelyReproofing() {
		return
	}
	deadline := time.Now().UTC().Add(budget)
	for {
		g.mu.Lock()
		installed := g.active != nil
		g.mu.Unlock()
		if installed && eligibility.Eligible() {
			return
		}
		// The last proof's lifetime can end during the wait; refuse then.
		if !eligibility.activelyReproofing() {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(min(5*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
