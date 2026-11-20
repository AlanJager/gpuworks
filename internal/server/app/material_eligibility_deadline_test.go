//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	eligibility.setOnInvalidated(func() {
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
