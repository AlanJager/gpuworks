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

type materialTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *materialTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *materialTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestMaterialEligibilityExpiresTenSecondsFromProofStart(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	var calls atomic.Int32
	eligibility := newMaterialEligibility(clock.Now, func(context.Context) (bool, error) {
		calls.Add(1)
		clock.Advance(4 * time.Second)
		return true, nil
	}, time.Second)
	if eligibility.Eligible() {
		t.Fatal("unproved member was eligible")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("proof: ready=%t err=%v", ready, err)
	}
	for range 100 {
		if !eligibility.Eligible() {
			t.Fatal("request check lost fresh eligibility")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("request check performed %d proofs", calls.Load())
	}
	clock.Advance(materialEligibilityWindow - 4*time.Second - time.Nanosecond)
	if !eligibility.Eligible() {
		t.Fatal("proof expired before the eligibility window")
	}
	clock.Advance(time.Nanosecond)
	if eligibility.Eligible() {
		t.Fatal("proof remained eligible at deadline")
	}
	clock.Advance(-time.Second)
	if eligibility.Eligible() {
		t.Fatal("clock rollback resurrected expired proof")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("fresh proof did not restore eligibility: ready=%t err=%v", ready, err)
	}
}

func TestMaterialEligibilityFailedRefreshKeepsUnexpiredProof(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	ready, proofErr := true, error(nil)
	proofDelay := time.Duration(0)
	eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		clock.Advance(proofDelay)
		return ready, proofErr
	}, 0)
	if got, err := eligibility.Reprove(t.Context()); err != nil || !got {
		t.Fatalf("initial proof: ready=%t err=%v", got, err)
	}
	proofErr = errors.New("no quorum")
	if got, err := eligibility.Reprove(t.Context()); got || !errors.Is(err, proofErr) || !eligibility.Eligible() {
		t.Fatalf("failed refresh discarded an unexpired proof: ready=%t err=%v eligible=%t", got, err, eligibility.Eligible())
	}
	clock.Advance(materialEligibilityWindow)
	if eligibility.Eligible() {
		t.Fatal("failed refresh extended eligibility past the original window")
	}
	proofErr = nil
	proofDelay = materialEligibilityWindow
	if got, err := eligibility.Reprove(t.Context()); got || err != nil || eligibility.Eligible() {
		t.Fatalf("slow proof opened eligibility: ready=%t err=%v", got, err)
	}
	proofDelay = 0
	if got, err := eligibility.Reprove(t.Context()); err != nil || !got {
		t.Fatalf("fresh proof: ready=%t err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := eligibility.Reprove(ctx); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled proof: ready=%t err=%v", got, err)
	}
	if eligibility.Eligible() {
		t.Fatal("canceled proof retained earlier eligibility")
	}
	if got, err := eligibility.Reprove(t.Context()); err != nil || !got {
		t.Fatalf("recovery proof: ready=%t err=%v", got, err)
	}
	eligibility.Invalidate()
	if eligibility.Eligible() {
		t.Fatal("explicit invalidation retained eligibility")
	}
}

func TestMaterialEligibilityInvalidationBlocksLateSuccess(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	eligibility := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release // deliberately ignores cancellation
		}
		return true, nil
	}, 0)
	done := make(chan bool, 1)
	go func() { ready, _ := eligibility.Reprove(t.Context()); done <- ready }()
	<-started
	eligibility.Invalidate()
	if eligibility.Eligible() {
		t.Fatal("invalidation did not close in-flight proof")
	}
	close(release)
	if <-done || eligibility.Eligible() {
		t.Fatal("late proof reopened after invalidation")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("new epoch failed to reprove: ready=%t err=%v", ready, err)
	}
	eligibility.Stop()
	if eligibility.Eligible() {
		t.Fatal("stopped App remained eligible")
	}
	if ready, err := eligibility.Reprove(t.Context()); ready || err != nil {
		t.Fatalf("stopped App reproved: ready=%t err=%v", ready, err)
	}
}

func TestMaterialEligibilityCanceledProofClosesBeforeSlowProverReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
		if calls.Add(1) == 2 {
			close(started)
			<-release // simulate a prover delayed in external I/O
		}
		return true, nil
	}, 0)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { ready, _ := eligibility.Reprove(ctx); done <- ready }()
	<-started
	cancel()
	if eligibility.Eligible() {
		t.Fatal("cancellation retained earlier proof while prover was blocked")
	}
	close(release)
	if <-done || eligibility.Eligible() {
		t.Fatal("canceled late proof reopened eligibility")
	}
}

func TestMaterialEligibilityRunRefreshesAndClosesOnCanceledLifetime(t *testing.T) {
	var calls atomic.Int32
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) {
		calls.Add(1)
		return true, nil
	}, time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eligibility.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for !eligibility.Eligible() || calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("background proof did not start and refresh")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if eligibility.Eligible() {
		t.Fatal("canceled lifetime did not close immediately")
	}
	<-done
	if ready, err := eligibility.Reprove(t.Context()); ready || err != nil {
		t.Fatalf("closed lifetime reproved: ready=%t err=%v", ready, err)
	}
}

func TestMaterialEligibilityGrantTimeoutKeepsUnexpiredProof(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	var calls atomic.Int32
	var invalidated atomic.Int32
	eligibility := newMaterialEligibility(clock.Now, func(context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		clock.Advance(storeGrantTimeout())
		return false, context.DeadlineExceeded
	}, time.Second)
	eligibility.setOnInvalidated(func() { invalidated.Add(1) })
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	ready, err := eligibility.Reprove(t.Context())
	if ready || !errors.Is(err, context.DeadlineExceeded) || !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("grant timeout withdrew an unexpired proof: ready=%t err=%v eligible=%t invalidated=%d", ready, err, eligibility.Eligible(), invalidated.Load())
	}
	clock.Advance(materialEligibilityWindow)
	if eligibility.Eligible() {
		t.Fatal("kept proof outlived its original window")
	}
}

func storeGrantTimeout() time.Duration {
	return materialEligibilityWindow - 10*time.Second
}

func TestMaterialEligibilityPeerIdleTimeoutKeepsAdmissionAcrossExpiredWindow(t *testing.T) {
	clock := &materialTestClock{now: time.Unix(100, 0)}
	var calls atomic.Int32
	var invalidated atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	eligibility := newMaterialEligibility(clock.Now, func(context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		close(entered)
		<-release
		return false, context.DeadlineExceeded
	}, time.Second)
	eligibility.setOnInvalidated(func() { invalidated.Add(1) })
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("initial proof: ready=%t err=%v", ready, err)
	}
	// Start the next attempt while the published proof still has time left,
	// then let the attempt outlive that proof. 15s is the peer idle bound.
	clock.Advance(materialEligibilityWindow - 5*time.Second)
	done := make(chan error, 1)
	go func() {
		ready, err := eligibility.Reprove(t.Context())
		if ready {
			err = errors.New("idle timeout reported ready")
		}
		done <- err
	}()
	<-entered
	clock.Advance(6 * time.Second)
	if !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("in-flight idle timeout closed ordinary admission: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
	}
	if eligibility.EligibleFor(storeGrantTimeout()) || invalidated.Load() != 0 {
		t.Fatalf("expired proof advertised grant headroom: eligible=%t invalidated=%d", eligibility.EligibleFor(storeGrantTimeout()), invalidated.Load())
	}
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("peer idle timeout: %v", err)
	}
	if !eligibility.Eligible() || invalidated.Load() != 0 {
		t.Fatalf("returned idle timeout closed ordinary admission: eligible=%t invalidated=%d", eligibility.Eligible(), invalidated.Load())
	}
	clock.Advance(materialEligibilityWindow)
	if eligibility.Eligible() {
		t.Fatal("attempt window stayed open for a full extra eligibility window")
	}
}
