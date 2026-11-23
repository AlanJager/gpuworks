//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Admission without an answer: what an attempt that brought no answer may
// keep, for how long, and how the deadline ends by itself.

// confirmedRecently reports whether the last successful proof is recent
// enough for a renewal without an answer to stand in for it. Called with
// e.mu held.
func (e *materialEligibility) confirmedRecently(now time.Time) bool {
	return !e.answered && !e.provedAt.IsZero() && now.Before(e.provedAt.Add(materialUnconfirmedLimit))
}

// materialProofUnanswered reports a proof error whose every cause is an
// expired deadline, a database with no leader to ask, or a leader not read in
// time. Nothing was compared, so it is no answer about this member.
func materialProofUnanswered(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		for _, cause := range causes {
			if !materialProofUnanswered(cause) {
				return false
			}
		}
		return len(causes) > 0
	}
	if err == context.DeadlineExceeded || err == store.ErrClusterMembershipObservationTimeout || store.ReplicatedLeaderUnavailable(err) {
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return materialProofUnanswered(wrapped.Unwrap())
	}
	return false
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
	var timer *time.Timer
	timer = time.AfterFunc(max(deadline.Sub(e.now()), 0), func() {
		e.mu.Lock()
		if e.expiry != timer {
			e.mu.Unlock()
			return
		}
		// The clock that sets deadlines may not be the timer's.
		if rest := e.deadline.Sub(e.now()); rest > 0 {
			e.admitUntil(e.deadline)
			e.mu.Unlock()
			return
		}
		e.expiry = nil
		e.mu.Unlock()
		e.Eligible()
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
