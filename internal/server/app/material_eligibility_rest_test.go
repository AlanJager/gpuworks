//go:build linux && dqlite

package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialEligibilityRestDeadlineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Time
		want     time.Duration
	}{
		{"full", time.Unix(115, 0), 2 * time.Second},
		{"partial", time.Unix(110, 500000000), 500 * time.Millisecond},
		{"two-grant-margin", time.Unix(110, 0), 0},
		{"expired", time.Unix(99, 0), 0},
		{"invalidated-zero", time.Time{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newMaterialEligibility(func() time.Time { return time.Unix(100, 0) }, nil, 2*time.Second)
			e.deadline = tc.deadline
			if got := e.successRest(); got != tc.want {
				t.Fatalf("rest=%s want=%s deadline=%s", got, tc.want, tc.deadline)
			}
			if e.deadline != tc.deadline {
				t.Fatal("scheduling changed deadline")
			}
		})
	}
}

func TestMaterialEligibilitySlowSuccessRestsBeforeNextProof(t *testing.T) {
	const interval = 200 * time.Millisecond
	clock := &materialTestClock{now: time.Unix(100, 0)}
	entered, release := make(chan struct{}), make(chan struct{})
	returned, next := make(chan time.Time, 1), make(chan time.Time, 1)
	var calls atomic.Int32
	e := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			clock.Advance(time.Second)
			returned <- time.Now()
			return true, nil
		}
		next <- time.Now()
		<-ctx.Done()
		return false, ctx.Err()
	}, interval)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial proof was not immediate")
	}
	// Ensure the original ticker already has a pending tick when proof succeeds.
	<-time.After(2 * interval)
	close(release)
	finished := <-returned
	select {
	case started := <-next:
		if started.Sub(finished) < interval*3/4 {
			t.Fatalf("slow successful proof reentered without rest: gap=%s interval=%s", started.Sub(finished), interval)
		}
	case <-time.After(4 * interval):
		t.Fatal("successful proof never resumed")
	}
	e.mu.Lock()
	deadline := e.deadline
	e.mu.Unlock()
	if deadline != time.Unix(115, 0) {
		t.Fatalf("rest redated proof-start lifetime: %s", deadline)
	}
}

func TestMaterialEligibilitySuccessRestUsesRemainingLifetime(t *testing.T) {
	const interval = 600 * time.Millisecond
	for _, tc := range []struct {
		name     string
		headroom time.Duration
	}{
		{"partial", 100 * time.Millisecond}, {"none", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &materialTestClock{now: time.Unix(100, 0)}
			var calls atomic.Int32
			returned, next := make(chan time.Time, 1), make(chan time.Time, 1)
			e := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
				if calls.Add(1) == 1 {
					clock.Advance(materialEligibilityWindow - 2*store.ClusterMembershipGrantTimeout - tc.headroom)
					returned <- time.Now()
					return true, nil
				}
				next <- time.Now()
				<-ctx.Done()
				return false, ctx.Err()
			}, interval)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.Run(ctx) }()
			defer func() { cancel(); <-done }()
			finished := <-returned
			select {
			case started := <-next:
				gap := started.Sub(finished)
				if tc.headroom > 0 && gap < tc.headroom/2 {
					t.Fatalf("partial headroom rest omitted: %s", gap)
				}
				if gap >= interval*3/4 {
					t.Fatalf("late success received unconditional interval: gap=%s headroom=%s", gap, tc.headroom)
				}
			case <-time.After(interval * 3 / 4):
				t.Fatal("late success slept full interval")
			}
		})
	}
}

func TestMaterialEligibilityRestCancellationAndInvalidation(t *testing.T) {
	const interval = 400 * time.Millisecond
	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "invalidate"}[invalidate], func(t *testing.T) {
			clock := &materialTestClock{now: time.Unix(100, 0)}
			var calls atomic.Int32
			returned, next := make(chan struct{}, 1), make(chan struct{}, 1)
			e := newMaterialEligibility(clock.Now, func(ctx context.Context) (bool, error) {
				if calls.Add(1) == 1 {
					returned <- struct{}{}
					return true, nil
				}
				next <- struct{}{}
				<-ctx.Done()
				return false, ctx.Err()
			}, interval)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.Run(ctx) }()
			defer func() { cancel(); <-done }()
			<-returned
			limit := time.After(time.Second)
			for !e.Eligible() {
				select {
				case <-limit:
					t.Fatal("success not published")
				case <-time.After(time.Millisecond):
				}
			}
			e.mu.Lock()
			deadline := e.deadline
			epoch := e.epoch
			e.mu.Unlock()
			if invalidate {
				select {
				case <-next:
					t.Fatal("success did not enter quiet interval")
				case <-time.After(interval / 8):
				}
				e.Invalidate(closeMaterialPass)
				if e.Eligible() {
					t.Fatal("invalidation did not close immediately")
				}
				select {
				case <-next:
					t.Fatal("invalidation bypassed successful rest")
				case <-time.After(interval / 4):
				}
				e.mu.Lock()
				bad := !e.deadline.IsZero() || e.epoch == epoch
				e.mu.Unlock()
				if bad {
					t.Fatal("rest revived old invalidated epoch")
				}
				select {
				case <-next:
				case <-time.After(2 * interval):
					t.Fatal("fresh proof did not resume after rest")
				}
				if e.Eligible() {
					t.Fatal("new unfinished proof revived invalidated eligibility")
				}
			} else {
				e.mu.Lock()
				unchanged := e.deadline == deadline
				e.mu.Unlock()
				if !unchanged {
					t.Fatal("rest extended deadline")
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(interval / 2):
				t.Fatal("rest ignored cancellation")
			}
		})
	}
}

func TestMaterialEligibilityFailureTickerAndSuccessResetCadence(t *testing.T) {
	const interval = 180 * time.Millisecond
	for _, successFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial-failure", true: "success-then-fast-failure"}[successFirst], func(t *testing.T) {
			var calls atomic.Int32
			failed, next := make(chan time.Time, 1), make(chan time.Time, 1)
			e := newMaterialEligibility(nil, func(ctx context.Context) (bool, error) {
				n := calls.Add(1)
				if successFirst && n == 1 {
					<-time.After(2 * interval)
					return true, nil
				}
				if (!successFirst && n == 1) || (successFirst && n == 2) {
					failed <- time.Now()
					return false, nil
				}
				next <- time.Now()
				<-ctx.Done()
				return false, ctx.Err()
			}, interval)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); e.Run(ctx) }()
			defer func() { cancel(); <-done }()
			finished := <-failed
			select {
			case started := <-next:
				if started.Sub(finished) < interval*2/3 {
					t.Fatalf("failure consumed stale success tick: gap=%s", started.Sub(finished))
				}
			case <-time.After(3 * interval):
				t.Fatal("failure retry never resumed")
			}
		})
	}
}
