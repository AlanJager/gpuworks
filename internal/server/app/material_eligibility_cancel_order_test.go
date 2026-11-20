//go:build linux && dqlite

package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestMaterialEligibilityCancelsProofBeforeInvalidationCallback(t *testing.T) {
	for _, action := range []string{"Invalidate", "Stop"} {
		t.Run(action, func(t *testing.T) {
			proofStarted := make(chan struct{})
			proofCanceled := make(chan struct{})
			proofRelease := make(chan struct{})
			callbackEntered := make(chan struct{}, 1)
			callbackRelease := make(chan struct{})
			calls := 0
			eligibility := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
				calls++
				if calls == 2 {
					close(proofStarted)
					<-ctx.Done()
					close(proofCanceled)
					<-proofRelease
				}
				return true, nil // A late success must not renew invalidated eligibility.
			}, time.Hour)
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial proof: ready=%t err=%v", ready, err)
			}
			eligibility.setOnInvalidated(func() {
				select {
				case callbackEntered <- struct{}{}:
				default:
				}
				<-callbackRelease
			})
			ctx, cancel := context.WithCancel(t.Context())
			proofDone := make(chan struct{})
			result := make(chan bool, 1)
			invalidationDone := make(chan struct{})
			var proofOnce, callbackOnce sync.Once
			releaseProof := func() { proofOnce.Do(func() { close(proofRelease) }) }
			releaseCallback := func() { callbackOnce.Do(func() { close(callbackRelease) }) }
			t.Cleanup(func() {
				cancel()
				releaseProof()
				releaseCallback()
				for _, done := range []<-chan struct{}{proofDone, invalidationDone} {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("proof or invalidation did not drain during cleanup")
					}
				}
			})
			go func() {
				defer close(proofDone)
				ready, _ := eligibility.Reprove(ctx)
				result <- ready
			}()
			go func() {
				defer close(invalidationDone)
				select {
				case <-proofStarted:
				case <-ctx.Done():
					return
				}
				if action == "Invalidate" {
					eligibility.Invalidate()
				} else {
					eligibility.Stop()
				}
			}()
			select {
			case <-callbackEntered:
			case <-time.After(time.Second):
				t.Fatal("invalidation callback did not enter")
			}
			if eligibility.Eligible() {
				t.Fatal("blocked invalidation callback retained eligibility")
			}
			select {
			case <-proofCanceled:
			case <-time.After(time.Second):
				t.Fatal("proof cancellation waited for invalidation callback")
			}
			releaseProof()
			releaseCallback()
			select {
			case ready := <-result:
				if ready || eligibility.Eligible() {
					t.Fatal("late proof success renewed invalidated eligibility")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled proof did not return after callback release")
			}
		})
	}
}
