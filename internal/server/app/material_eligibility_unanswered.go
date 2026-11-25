//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"
)

// Admission without an answer: what an attempt that brought no answer may
// keep, for how long, and how the deadline ends by itself.

// confirmedRecently reports whether the last successful proof is recent
// enough for a renewal without an answer to stand in for it. Called with
// e.mu held.
func (e *materialEligibility) confirmedRecently(now time.Time) bool {
	return !e.answered && !e.provedAt.IsZero() && now.Before(e.provedAt.Add(materialUnconfirmedLimit))
}

// closeAdmission ends admission on an answer, an invalidation or Stop.
// Called with e.mu held.
func (e *materialEligibility) closeAdmission() {
	e.answered = true
	e.admitUntil(time.Time{})
}

// admitUntil sets the deadline and arms its expiry, so admission ends at the
// deadline even when no request asks and the proof in progress cannot be
// interrupted. The zero time ends admission now. Called with e.mu held.
func (e *materialEligibility) admitUntil(deadline time.Time) {
	e.deadline = deadline
	if e.expiry != nil {
		e.expiry.Stop()
		e.expiry = nil
	}
	if deadline.IsZero() || e.stopped {
		return
	}
	e.armExpiry(deadline.Sub(e.now()))
}

// expiryRecheck is how soon an expiry that found admission still carried by a
// proof in progress looks again.
const expiryRecheck = time.Second

// armExpiry is called with e.mu held.
func (e *materialEligibility) armExpiry(after time.Duration) {
	var timer *time.Timer
	timer = time.AfterFunc(max(after, 0), func() {
		e.mu.Lock()
		if e.expiry != timer {
			e.mu.Unlock()
			return
		}
		// The clock that sets deadlines may not be the timer's.
		if rest := e.deadline.Sub(e.now()); rest > 0 {
			e.armExpiry(rest)
			e.mu.Unlock()
			return
		}
		e.expiry = nil
		e.mu.Unlock()
		// The request-path check latches the expiry and runs the close.
		e.Eligible()
		// A proof in progress may carry admission past the deadline; then
		// nothing was latched, and the close must not wait for a request.
		e.mu.Lock()
		if !e.deadline.IsZero() && !e.stopped && e.expiry == nil {
			e.armExpiry(expiryRecheck)
		}
		e.mu.Unlock()
	})
	e.expiry = timer
}

// outlivedWindow reports a proof that ended only because its own window ran
// out: nobody cancelled it, invalidated it or stopped this component. Called
// with e.mu held.
func (e *materialEligibility) outlivedWindow(parent, proofCtx context.Context, epoch uint64) bool {
	return errors.Is(proofCtx.Err(), context.DeadlineExceeded) && parent.Err() == nil &&
		e.epoch == epoch && !e.stopped && (e.runCtx == nil || e.runCtx.Err() == nil)
}

// undisturbed reports a proof nobody cancelled, invalidated or stopped and
// whose context has not ended by itself either: the caller knows by the clock
// that its window is over. Called with e.mu held.
func (e *materialEligibility) undisturbed(parent, proofCtx context.Context, epoch uint64) bool {
	return proofCtx.Err() == nil && parent.Err() == nil &&
		e.epoch == epoch && !e.stopped && (e.runCtx == nil || e.runCtx.Err() == nil)
}

// unconfirmedDeadline is how long admission continues after an attempt that
// brought no answer: one more proof window, never past the unconfirmed limit
// counted from the last successful proof. Called with e.mu held.
func (e *materialEligibility) unconfirmedDeadline(now time.Time) time.Time {
	deadline := now.Add(materialEligibilityWindow)
	if limit := e.provedAt.Add(materialUnconfirmedLimit); limit.Before(deadline) {
		deadline = limit
	}
	return deadline
}
