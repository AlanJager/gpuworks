//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"syscall"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialReplicationFailedCycleRetriesBeforeNormalTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), materialReproofInterval+time.Second)
	defer cancel()
	started := time.Now()
	if !waitForMaterialReplicationCycle(ctx, make(chan time.Time), time.Minute, true) {
		t.Fatal("failed complete cycle waited for the normal minute tick instead of retrying")
	}
	if elapsed := time.Since(started); elapsed < materialReproofInterval {
		t.Fatalf("failed cycle skipped its retry delay: %s", elapsed)
	}
}

func TestMaterialReplicationRepeatedFailuresUseShorterIntervalWithoutSpin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	const interval = 35 * time.Millisecond
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	started := time.Now()
	for range 3 {
		if !waitForMaterialReplicationCycle(ctx, tick, interval, true) {
			t.Fatal("failed cycle did not use the shorter configured interval")
		}
	}
	if elapsed := time.Since(started); elapsed < 3*interval {
		t.Fatalf("repeated failures spun on a pending normal tick: %s", elapsed)
	}
}

func TestMaterialReplicationSuccessfulCycleRetainsOriginalTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tick := make(chan time.Time, 1)
	done := make(chan bool, 1)
	go func() { done <- waitForMaterialReplicationCycle(ctx, tick, 35*time.Millisecond, false) }()
	select {
	case ready := <-done:
		t.Fatalf("successful cycle invented a retry wakeup: %t", ready)
	case <-time.After(80 * time.Millisecond):
	}
	tick <- time.Now()
	select {
	case ready := <-done:
		if !ready {
			t.Fatal("normal tick did not schedule the next successful cycle")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestMaterialReplicationRetryWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- waitForMaterialReplicationCycle(ctx, make(chan time.Time), time.Minute, true) }()
	cancel()
	select {
	case ready := <-done:
		if ready {
			t.Fatal("canceled failure scheduled another cycle")
		}
	case <-time.After(time.Second):
		t.Fatal("retry wait did not stop on cancellation")
	}
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	if waitForMaterialReplicationCycle(ctx, tick, time.Minute, false) {
		t.Fatal("an already queued normal tick overrode cancellation")
	}
}

func TestMaterialReplicationPassClosesMemberOnlyOnVerdict(t *testing.T) {
	integrity := errors.New("material digest differs")
	for _, tc := range []struct {
		name   string
		err    error
		closes bool
	}{
		{"pass ran out of time", context.DeadlineExceeded, false},
		{"store call ran out of time", fmt.Errorf("index material inventory locally: %w", context.DeadlineExceeded), false},
		{"no database leader", fmt.Errorf("grant: %w", dqlitedriver.ErrNoAvailableLeader), false},
		{"peer unreachable", &url.Error{Op: "Post", URL: "https://fixture.invalid", Err: syscall.ECONNREFUSED}, false},
		{"changed bytes", integrity, true},
		{"timeout joined to changed bytes", errors.Join(context.DeadlineExceeded, integrity), true},
		{"admission rejected", fmt.Errorf("grant: %w", store.ErrClusterAdmissionRejected), true},
		{"connection lost with a commit outstanding", driver.ErrBadConn, false},
		{"deadline with a commit outstanding", fmt.Errorf("commit manifest: %w", context.DeadlineExceeded), false},
		{"lost connection joined to changed bytes", errors.Join(driver.ErrBadConn, integrity), true},
		{"cancellation", context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			gate := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, materialReproofInterval)
			gate.setOnInvalidated(func() { closed++ })
			if ready, err := gate.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			a := &App{memberState: memberState{materialEligibility: gate}}
			a.materialReplicationPassFailed(tc.err)
			if tc.closes && (closed != 1 || gate.Eligible()) {
				t.Fatalf("verdict left the member open: closed=%d eligible=%v", closed, gate.Eligible())
			}
			if !tc.closes && (closed != 0 || !gate.Eligible()) {
				t.Fatalf("undecided failure closed the member: closed=%d eligible=%v", closed, gate.Eligible())
			}
		})
	}
}
