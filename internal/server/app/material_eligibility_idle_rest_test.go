//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The production worker rests longer after a success than it waits after a
// failure; every other user of the two-second interval is unchanged.
func TestCandidateEligibilityRestsLongerAfterSuccessThanItRetries(t *testing.T) {
	e := newCandidateMaterialEligibility(&App{})
	if e.interval != materialReproofInterval || e.idleRest != materialIdleReproofRest {
		t.Fatalf("interval=%s idleRest=%s, want %s and %s", e.interval, e.idleRest, materialReproofInterval, materialIdleReproofRest)
	}
	if materialIdleReproofRest <= materialReproofInterval {
		t.Fatalf("idle rest %s does not exceed the retry interval %s", materialIdleReproofRest, materialReproofInterval)
	}
	if limit := materialEligibilityWindow - 2*store.ClusterMembershipGrantTimeout; materialIdleReproofRest > limit {
		t.Fatalf("idle rest %s exceeds %s, beyond which successRest caps it", materialIdleReproofRest, limit)
	}
	if other := newMaterialEligibility(nil, nil, 0); other.idleRest != materialReproofInterval {
		t.Fatalf("a worker with no idle rest set rests %s, want its interval", other.idleRest)
	}
}

// proofStep is one scripted proof attempt of the schedule below.
type proofStep struct {
	takes time.Duration
	ready bool
	err   error
}

// runProofSchedule drives the worker's own scheduling decisions on a fake
// clock: Reprove, then successRest after a success or the retry interval
// (measured from the attempt's start) after a failure, exactly as Run does.
// before is called at the end of every attempt, just before its result is
// published, which is where the earlier proof has the least lifetime left.
func runProofSchedule(t *testing.T, steps []proofStep, before func(step int, e *materialEligibility)) *materialEligibility {
	t.Helper()
	clock := &materialTestClock{now: time.Unix(1000, 0)}
	var mu sync.Mutex
	step := 0
	var e *materialEligibility
	e = newMaterialEligibility(clock.Now, func(context.Context) (bool, error) {
		mu.Lock()
		current := step
		mu.Unlock()
		clock.Advance(steps[current].takes)
		if before != nil {
			before(current, e)
		}
		return steps[current].ready, steps[current].err
	}, materialReproofInterval)
	e.setIdleRest(materialIdleReproofRest)
	for i, scripted := range steps {
		mu.Lock()
		step = i
		mu.Unlock()
		ready, err := e.Reprove(t.Context())
		if ready != (scripted.ready && scripted.err == nil) {
			t.Fatalf("attempt %d ready=%v err=%v, scripted %+v", i, ready, err, scripted)
		}
		if ready {
			clock.Advance(e.successRest())
			continue
		}
		if wait := e.interval - scripted.takes; wait > 0 {
			clock.Advance(wait)
		}
	}
	return e
}

// One slow proof, or one failed attempt followed by a good proof, must never
// leave less than the grant timeout that readiness requires.
func TestIdleRestKeepsReadinessHeadroomThroughOneBadAttempt(t *testing.T) {
	required := store.ClusterMembershipGrantTimeout
	failed := errors.New("interrupted")
	for _, first := range []time.Duration{100 * time.Millisecond, time.Second, 3 * time.Second, required} {
		for _, tc := range []struct {
			name  string
			steps []proofStep
		}{
			{"renewal-as-slow-as-a-grant-timeout", []proofStep{{first, true, nil}, {required, true, nil}}},
			{"fast-failure-then-three-second-proof", []proofStep{{first, true, nil}, {100 * time.Millisecond, false, failed}, {3 * time.Second, true, nil}}},
			{"not-ready-then-three-second-proof", []proofStep{{first, true, nil}, {materialReproofInterval, false, nil}, {3 * time.Second, true, nil}}},
		} {
			t.Run(tc.name+"/first="+first.String(), func(t *testing.T) {
				last := len(tc.steps) - 1
				e := runProofSchedule(t, tc.steps, func(step int, e *materialEligibility) {
					if step == 0 {
						return
					}
					if ready, reason, remaining := e.eligibleForResult(required); !ready {
						t.Fatalf("attempt %d ends with %s of the earlier proof left (%s), want at least %s", step, remaining, reason, required)
					}
				})
				if !e.EligibleFor(required) || last < 1 {
					t.Fatal("the renewed proof does not carry readiness headroom")
				}
			})
		}
	}
}

// The reserve covers one bad attempt, not two: readiness headroom is lost
// while ordinary admission continues behind the renewal in flight, and
// admission closes once the lifetime ends with no proof that succeeded.
func TestIdleRestDoesNotCoverTwoFailedAttempts(t *testing.T) {
	required := store.ClusterMembershipGrantTimeout
	failed := errors.New("interrupted")
	below := false
	runProofSchedule(t, []proofStep{
		{time.Second, true, nil},
		{100 * time.Millisecond, false, failed},
		{100 * time.Millisecond, false, failed},
		{3 * time.Second, true, nil},
	}, func(step int, e *materialEligibility) {
		if step != 3 {
			return
		}
		below = !e.EligibleFor(required)
		if !e.Eligible() {
			t.Fatal("ordinary admission closed while the earlier proof was still inside its lifetime")
		}
	})
	if !below {
		t.Fatal("two failed attempts and a three-second proof kept readiness headroom; the documented reserve is wrong")
	}

	steps := []proofStep{{time.Second, true, nil}}
	for i := 0; i < 8; i++ {
		steps = append(steps, proofStep{100 * time.Millisecond, false, failed})
	}
	e := runProofSchedule(t, steps, nil)
	if e.Eligible() {
		t.Fatal("admission stayed open after every renewal failed for the whole proof lifetime")
	}
}

// Run itself applies the two cadences: the long rest after a success and the
// short interval after a failure.
func TestMaterialEligibilityRunRestsAfterSuccessAndRetriesAfterFailure(t *testing.T) {
	const interval, rest = 50 * time.Millisecond, 600 * time.Millisecond
	starts := make(chan time.Time, 8)
	var mu sync.Mutex
	calls := 0
	e := newMaterialEligibility(nil, func(context.Context) (bool, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		starts <- time.Now()
		// success, failure, success: the gaps are rest, then interval.
		return n != 2, nil
	}, interval)
	e.setIdleRest(rest)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.Run(ctx)
	var at [3]time.Time
	for i := range at {
		select {
		case at[i] = <-starts:
		case <-time.After(10 * time.Second):
			t.Fatalf("proof %d did not start", i+1)
		}
	}
	if gap := at[1].Sub(at[0]); gap < rest-20*time.Millisecond {
		t.Fatalf("the proof after a success started after %s, want the %s rest", gap, rest)
	}
	if gap := at[2].Sub(at[1]); gap >= rest-100*time.Millisecond {
		t.Fatalf("the retry after a failed proof started after %s, want the %s interval, not the rest", gap, interval)
	}
}
