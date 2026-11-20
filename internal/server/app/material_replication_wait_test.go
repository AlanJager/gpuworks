//go:build linux && dqlite

package app

import (
	"context"
	"testing"
	"time"
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
