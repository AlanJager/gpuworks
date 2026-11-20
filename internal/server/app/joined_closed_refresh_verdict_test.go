//go:build linux && dqlite

package app

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A commit stall makes several refresh cycles in a row fail while each cycle
// is still live. The member keeps serving, the refresh loop repeats on its
// own cadence, and the cycle that gets through changes nothing either.
func TestClosedTransportRefreshLoopKeepsBusinessOpenThroughUndecidedFailures(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	var closures atomic.Int64
	eligibility.setOnInvalidated(func() { closures.Add(1) })
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%v err=%v", ready, err)
	}
	failures := []error{
		dqlitedriver.Error{Code: 3, Message: "key"},
		dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"},
		dqlitedriver.ErrNoAvailableLeader,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int64
	recovered := make(chan struct{})
	refresh := a.closedTransportAuthorizationRefresh(func(context.Context) error {
		n := attempts.Add(1)
		if !eligibility.Eligible() {
			t.Errorf("attempt %d started on a closed member", n)
		}
		if int(n) <= len(failures) {
			return failures[n-1]
		}
		if int(n) == len(failures)+1 {
			close(recovered)
		}
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTransportAuthorization(ctx, refresh, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Minute)
	}()
	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatalf("refresh loop made %d attempts and never got past the failures", attempts.Load())
	}
	cancel()
	<-done
	if !eligibility.Eligible() || closures.Load() != 0 {
		t.Fatalf("undecided refresh failures closed the member: eligible=%v closures=%d", eligibility.Eligible(), closures.Load())
	}
}

// A refusal closes on the first cycle that returns it, with the cycle live.
func TestClosedTransportRefreshRefusalClosesAtOnce(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	var closures atomic.Int64
	eligibility.setOnInvalidated(func() { closures.Add(1) })
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%v err=%v", ready, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	refresh := a.closedTransportAuthorizationRefresh(func(context.Context) error { return store.ErrClusterAdmissionRejected })
	if err := refresh(ctx); err == nil || eligibility.Eligible() || closures.Load() == 0 {
		t.Fatalf("refusal kept the member open: err=%v eligible=%v closures=%d", err, eligibility.Eligible(), closures.Load())
	}
}

// Keeping the proof through undecided failures does not extend it: when no
// proof succeeds either, the member closes at the end of the proof's window.
func TestClosedTransportRefreshUndecidedFailuresDoNotOutliveTheProofWindow(t *testing.T) {
	var clock atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return base.Add(time.Duration(clock.Load())) }
	eligibility := newMaterialEligibility(now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	var closures atomic.Int64
	eligibility.setOnInvalidated(func() { closures.Add(1) })
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%v err=%v", ready, err)
	}
	refresh := a.closedTransportAuthorizationRefresh(func(context.Context) error {
		return dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}
	})
	for elapsed := time.Second; elapsed < materialEligibilityWindow; elapsed += time.Second {
		clock.Store(int64(elapsed))
		if err := refresh(t.Context()); err == nil {
			t.Fatal("refresh failure was not returned")
		}
		if !eligibility.Eligible() {
			t.Fatalf("member closed %v into a %v proof window", elapsed, materialEligibilityWindow)
		}
	}
	clock.Store(int64(materialEligibilityWindow))
	if err := refresh(t.Context()); err == nil {
		t.Fatal("refresh failure was not returned")
	}
	if eligibility.Eligible() || closures.Load() != 1 {
		t.Fatalf("member stayed open past the proof window: eligible=%v closures=%d", eligibility.Eligible(), closures.Load())
	}
}
