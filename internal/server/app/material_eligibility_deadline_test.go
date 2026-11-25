//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialEligibilityDeadlineCancelsProofAndDrainsBeforeRecovery(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	proofStarted := make(chan context.Context, 1)
	proofCanceled := make(chan struct{})
	proofRelease := make(chan struct{})
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	firstDone := make(chan struct{})
	recoveryDone := make(chan struct{})
	var calls atomic.Int32
	var callbackStartOnce sync.Once
	var recoveryStarted bool
	eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 2 {
			proofStarted <- ctx
			<-ctx.Done()
			close(proofCanceled)
			<-proofRelease // Cancellation cannot stand in for draining actual I/O.
		}
		return true, nil // A late success must not renew the expired proof.
	}, time.Hour)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	// The last confirmation is older than the unconfirmed limit, so a proof
	// that outlives its window has nothing recent to stand on and closes.
	clock.Advance(materialUnconfirmedLimit)
	eligibility.setOnInvalidated(func(admissionCloseCause) {
		callbackStartOnce.Do(func() { close(callbackStarted) })
		<-callbackRelease
	})
	ctx, cancel := context.WithCancel(t.Context())
	var proofOnce, callbackOnce sync.Once
	releaseProof := func() { proofOnce.Do(func() { close(proofRelease) }) }
	releaseCallback := func() { callbackOnce.Do(func() { close(callbackRelease) }) }
	t.Cleanup(func() {
		cancel()
		releaseProof()
		releaseCallback()
		select {
		case <-firstDone:
		case <-time.After(time.Second):
			t.Error("expired proof did not drain during cleanup")
		}
		if recoveryStarted {
			select {
			case <-recoveryDone:
			case <-time.After(time.Second):
				t.Error("recovery proof did not drain during cleanup")
			}
		}
	})
	go func() {
		defer close(firstDone)
		ready, err := eligibility.Reprove(ctx)
		if ready || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expired proof: ready=%t err=%v", ready, err)
		}
	}()
	proofCtx := <-proofStarted
	if deadline, ok := proofCtx.Deadline(); !ok || time.Until(deadline) > materialEligibilityWindow {
		t.Fatal("proof I/O has no deadline within its useful eligibility window")
	}
	select {
	case <-proofCanceled:
	case <-time.After(materialEligibilityWindow + time.Second):
		t.Fatal("expired proof I/O was not canceled")
	}
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("deadline waited for the prover to return before notifying invalidation")
	}
	eligibility.mu.Lock()
	closed := eligibility.deadline.IsZero()
	eligibility.mu.Unlock()
	if !closed {
		t.Fatal("deadline callback did not close eligibility while prover was blocked")
	}
	recoveryStarted = true
	go func() {
		defer close(recoveryDone)
		ready, err := eligibility.Reprove(t.Context())
		if err != nil || !ready || !eligibility.Eligible() {
			t.Errorf("fresh proof did not restore eligibility: ready=%t err=%v", ready, err)
		}
	}()
	select {
	case <-recoveryDone:
		t.Fatal("recovery bypassed the expired proof's undrained I/O")
	case <-time.After(20 * time.Millisecond):
	}
	releaseProof()
	select {
	case <-recoveryDone:
		t.Fatal("recovery bypassed the expired proof's invalidation callback")
	case <-time.After(20 * time.Millisecond):
	}
	releaseCallback()
	for _, done := range []<-chan struct{}{firstDone, recoveryDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("proof did not return after I/O and callback were drained")
		}
	}
	if calls.Load() != 3 || !eligibility.Eligible() {
		t.Fatal("expired callback invalidated the replacement proof")
	}
}

// A proof that only outlives its own window says nothing about this member.
// With a recent confirmation behind it the member stays open, the proof's
// I/O is still cancelled, its late success publishes nothing, and admission
// ends at the unconfirmed limit unless a later proof succeeds.
func TestMaterialEligibilityProofOutlivingItsWindowKeepsRecentlyConfirmedMemberOpen(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	var calls, invalidated atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 2 {
			close(entered)
			<-ctx.Done()
			<-release
		}
		return true, nil
	}, time.Hour)
	eligibility.setOnInvalidated(func(admissionCloseCause) { invalidated.Add(1) })
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	clock.Advance(materialEligibilityWindow - time.Second)
	done := make(chan error, 1)
	go func() {
		ready, err := eligibility.Reprove(t.Context())
		if ready {
			err = errors.New("a proof that outlived its window reported ready")
		}
		done <- err
	}()
	<-entered
	eligibility.mu.Lock()
	proofCtx := eligibility.proofCtx
	eligibility.mu.Unlock()
	// The published proof expires while the attempt is still inside its window.
	clock.Advance(materialEligibilityWindow)
	select {
	case <-proofCtx.Done():
	case <-time.After(materialEligibilityWindow + time.Second):
		t.Fatal("proof I/O was not cancelled at the end of its window")
	}
	// The request-path check decides this by itself, whether or not the
	// proof's own cancellation callback has run yet.
	if !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("window expiry closed a recently confirmed member while the prover drained: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
	}
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outlived proof returned %v", err)
	}
	if !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("returned proof closed a recently confirmed member: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
	}
	if eligibility.EligibleFor(storeGrantTimeout() + materialEligibilityWindow) {
		t.Fatal("late success was published as a fresh proof")
	}
	// Nothing confirms the member again: admission ends at the limit.
	clock.Advance(materialUnconfirmedLimit)
	if eligibility.Eligible() || invalidated.Load() != 1 {
		t.Fatalf("member stayed open past the unconfirmed limit: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("a later proof did not reopen the member: ready=%t err=%v", ready, err)
	}
}

// An answer other than success is never forgotten: a refusal by the database
// closes at once, also while an earlier proof is still inside its window; any
// other answer leaves that proof its own window and no more. In both cases an
// attempt that brings no answer extends nothing afterwards, and only a
// successful proof reopens the member.
func TestMaterialEligibilityAnswerInsideWindowIsLatched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		atOnce bool
		answer func(*materialEligibility) (bool, error)
	}{
		{"refusal by the database", true, func(*materialEligibility) (bool, error) {
			return false, fmt.Errorf("grant: %w", store.ErrClusterAdmissionRejected)
		}},
		{"identity conflict joined to a timeout", true, func(*materialEligibility) (bool, error) {
			return false, errors.Join(context.DeadlineExceeded, store.ErrClusterIdentityConflict)
		}},
		{"invalidation during the proof", true, func(e *materialEligibility) (bool, error) {
			e.Invalidate(closeMaterialPass)
			return false, context.DeadlineExceeded
		}},
		{"not ready", false, func(*materialEligibility) (bool, error) { return false, nil }},
		{"unclassified error", false, func(*materialEligibility) (bool, error) { return false, errors.New("no quorum") }},
		{"changed bytes joined to a timeout", false, func(*materialEligibility) (bool, error) {
			return false, errors.Join(context.DeadlineExceeded, errors.New("material digest differs"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &materialTestClock{now: time.Unix(100, 0)}
			var calls atomic.Int32
			var eligibility *materialEligibility
			eligibility = newMaterialEligibility(clock.Now, func(context.Context) (bool, error) {
				switch calls.Add(1) {
				case 1, 5:
					return true, nil
				case 2:
					return tc.answer(eligibility)
				default:
					return false, fmt.Errorf("grant: %w", context.DeadlineExceeded)
				}
			}, time.Hour)
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial proof: ready=%t err=%v", ready, err)
			}
			clock.Advance(4 * time.Second)
			if ready, _ := eligibility.Reprove(t.Context()); ready {
				t.Fatal("answering attempt reported ready")
			}
			if open := eligibility.Eligible(); open == tc.atOnce {
				t.Fatalf("eligible=%t right after the answer, want %t", open, !tc.atOnce)
			}
			// Two attempts without an answer, the second past the first
			// proof's window: neither extends or reopens.
			clock.Advance(3 * time.Second)
			if ready, err := eligibility.Reprove(t.Context()); ready || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("attempt without an answer: ready=%t err=%v", ready, err)
			}
			if open := eligibility.Eligible(); open == tc.atOnce {
				t.Fatalf("eligible=%t inside the first window after an attempt without an answer, want %t", open, !tc.atOnce)
			}
			clock.Advance(materialEligibilityWindow - 7*time.Second)
			if eligibility.Eligible() {
				t.Fatal("member stayed open past the window of its last successful proof")
			}
			if ready, _ := eligibility.Reprove(t.Context()); ready || eligibility.Eligible() {
				t.Fatal("an attempt without an answer reopened a closed member")
			}
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
				t.Fatalf("a successful proof did not reopen the member: ready=%t err=%v", ready, err)
			}
			// With a fresh confirmation behind it, no answer keeps it again.
			clock.Advance(materialEligibilityWindow + time.Second)
			if _, err := eligibility.Reprove(t.Context()); !errors.Is(err, context.DeadlineExceeded) || !eligibility.Eligible() {
				t.Fatalf("attempt without an answer closed a freshly confirmed member: err=%v", err)
			}
		})
	}
}

// Admission ends at its deadline without any request asking.
func TestMaterialEligibilityExpiresAtItsDeadlineWithoutARequest(t *testing.T) {
	var invalidated atomic.Int32
	eligibility := newMaterialEligibility(nil, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	eligibility.setOnInvalidated(func(admissionCloseCause) { invalidated.Add(1) })
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	eligibility.mu.Lock()
	eligibility.admitUntil(eligibility.now().Add(50 * time.Millisecond))
	eligibility.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for invalidated.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if invalidated.Load() != 1 {
		t.Fatalf("deadline passed without closing admission: invalidated=%d", invalidated.Load())
	}
	eligibility.Stop()
}

// A proof can return after its window while its context's own timer has not
// run yet: its stages time out at the window's end, or the process was not
// scheduled across it (seen on the real hosts: 15.8 s standing still, the
// member closed about 50 s after its last confirmation instead of at the
// limit). With no answer, in every shape that is no answer, the member stays
// open on its last confirmation; an answer that arrives late closes it; and
// at the limit it closes once.
func TestMaterialEligibilityProofReturningPastItsWindowBeforeItsContextEndsKeepsTheMember(t *testing.T) {
	late := []struct {
		name  string
		ready bool
		err   error
		keep  bool
	}{
		// The reply had arrived before the process stood still and is read
		// after the window: a success that proves nothing about now.
		{"late success", true, nil, true},
		{"late not ready", false, nil, false},
		{"deadline", false, context.DeadlineExceeded, true},
		{"wrapped deadline", false, fmt.Errorf("membership grant: %w", context.DeadlineExceeded), true},
		{"joined deadlines", false, errors.Join(context.DeadlineExceeded, fmt.Errorf("transport grant: %w", context.DeadlineExceeded)), true},
		{"dropped connection", false, driver.ErrBadConn, true},
		{"no leader read in time", false, store.ErrClusterMembershipObservationTimeout, true},
		{"an unclassified error", false, errors.New("material manifest differs"), false},
		{"deadline joined with an answer", false, errors.Join(context.DeadlineExceeded, errors.New("material manifest differs")), false},
	}
	for _, tc := range late {
		t.Run(tc.name, func(t *testing.T) {
			clock := &materialTestClock{now: time.Unix(100, 0)}
			var calls, invalidated atomic.Int32
			var lateErr atomic.Pointer[error]
			var lateReady atomic.Bool
			var jump atomic.Int64
			eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
				if calls.Add(1) == 1 {
					return true, nil
				}
				// The clock passes the window; the context has not ended.
				clock.Advance(time.Duration(jump.Load()))
				if ctx.Err() != nil {
					t.Error("the proof context ended; this case needs it alive")
				}
				return lateReady.Load(), *lateErr.Load()
			}, time.Hour)
			eligibility.setOnInvalidated(func(admissionCloseCause) { invalidated.Add(1) })
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial proof: ready=%t err=%v", ready, err)
			}
			confirmed := clock.Now()
			// Unanswered proofs for 30 s, as on the hosts, each inside its window.
			unanswered := error(context.DeadlineExceeded)
			lateErr.Store(&unanswered)
			if tc.keep {
				jump.Store(int64(5 * time.Second))
				for range 6 {
					if ready, _ := eligibility.Reprove(t.Context()); ready {
						t.Fatal("an unanswered proof reported ready")
					}
				}
				if !eligibility.Eligible() || invalidated.Load() != 0 {
					t.Fatalf("unanswered proofs inside their windows closed the member: invalidated=%d", invalidated.Load())
				}
			}
			// The next one returns 16 s after it began.
			lateErr.Store(&tc.err)
			lateReady.Store(tc.ready)
			jump.Store(int64(materialEligibilityWindow + time.Second))
			if ready, _ := eligibility.Reprove(t.Context()); ready {
				t.Fatal("a proof that returned past its window reported ready")
			}
			if !tc.keep {
				if eligibility.Eligible() || invalidated.Load() != 1 {
					t.Fatalf("a late answer left the member open: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
				}
				return
			}
			if since := clock.Now().Sub(confirmed); since >= materialUnconfirmedLimit {
				t.Fatalf("the scenario ran %s, past the limit it is meant to stay under", since)
			}
			if !eligibility.Eligible() || invalidated.Load() != 0 {
				t.Fatalf("a proof that only returned late closed the member %s after its last confirmation: eligible=%t invalidated=%d",
					clock.Now().Sub(confirmed), eligibility.Eligible(), invalidated.Load())
			}
			// Nothing earlier than the limit ends it, and the limit ends it once.
			clock.Advance(confirmed.Add(materialUnconfirmedLimit).Sub(clock.Now()) - time.Millisecond)
			if !eligibility.Eligible() || invalidated.Load() != 0 {
				t.Fatalf("the member closed before the limit: invalidated=%d", invalidated.Load())
			}
			clock.Advance(time.Millisecond)
			if eligibility.Eligible() || eligibility.Eligible() || invalidated.Load() != 1 {
				t.Fatalf("at the limit: eligible=%t invalidated=%d, want closed once", eligibility.Eligible(), invalidated.Load())
			}
			// Another late return past the limit extends nothing.
			jump.Store(int64(materialEligibilityWindow + time.Second))
			if ready, _ := eligibility.Reprove(t.Context()); ready || eligibility.Eligible() {
				t.Fatal("a late return past the limit reopened the member")
			}
		})
	}
}
