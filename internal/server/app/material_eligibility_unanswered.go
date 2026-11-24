//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
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
// expired deadline, a database with no leader to ask, a leader not read in
// time, or a connection lost under the question. Nothing was compared, so it
// is no answer about this member.
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
	// A connection the driver dropped or the peer closed under a leadership
	// change delivered no answer either. These are compared as the sentinels
	// they are: a file's error carries a system call number that also passes
	// for a network error, and a file that cannot be read is a finding.
	if err == context.DeadlineExceeded || err == store.ErrClusterMembershipObservationTimeout || store.ReplicatedLeaderUnavailable(err) ||
		err == driver.ErrBadConn || err == io.EOF || err == io.ErrUnexpectedEOF {
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
	e.armExpiry(deadline.Sub(e.now()))
}

// expiryRecheck is how soon an expiry that found admission still carried by a
// proof in progress looks again. Called with e.mu held.
const expiryRecheck = time.Second

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
