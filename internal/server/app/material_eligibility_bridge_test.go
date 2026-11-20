//go:build linux && dqlite

package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaterialEligibilityStaysOpenWhileSlowRenewalIsInFlight(t *testing.T) {
	start := time.Unix(1_000, 0)
	now := start
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	eligibility := newMaterialEligibility(func() time.Time { return now }, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		close(entered)
		<-release
		return true, nil
	}, materialReproofInterval)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("initial proof: ready=%t err=%v eligible=%t", ready, err, eligibility.Eligible())
	}
	now = start.Add(materialEligibilityWindow - time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = eligibility.Reprove(t.Context())
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("renewal did not start")
	}
	now = start.Add(materialEligibilityWindow + time.Second)
	if !eligibility.Eligible() {
		t.Fatal("slow renewal closed admission before its own proof finished")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal did not finish")
	}
	if !eligibility.Eligible() {
		t.Fatal("successful renewal did not restore admission")
	}
}
