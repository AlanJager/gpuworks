//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Only scheduling is controlled. Full-byte reads use actual pinned HTTPS,
// and native tests separately retain every real authorization increment.
func TestMaterialWarmWaitBudgetRepeatedValidRenewalsAndNextGroup(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	entered := make(chan context.Context, 1)
	var calls atomic.Int32
	e := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		n := calls.Add(1)
		if n == 1 {
			return true, nil
		}
		entered <- ctx
		release := firstRelease
		if n == 3 {
			release = secondRelease
		}
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}, materialReproofInterval)
	if ready, err := e.Reprove(ctx); !ready || err != nil {
		t.Fatalf("initial scheduling proof ready=%v err=%v", ready, err)
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	var proofs sync.WaitGroup
	proofs.Add(1)
	go func() { defer proofs.Done(); _, err := e.Reprove(ctx); firstDone <- err }()
	firstProof := <-entered
	firstWait, secondWait, nextWait := make(chan struct{}), make(chan struct{}), make(chan struct{})
	secondCheck, nextCheck, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var capture atomic.Int32
	e.mu.Lock()
	e.now = func() time.Time {
		switch capture.Swap(0) {
		case 1:
			close(firstWait)
		case 3:
			close(secondWait)
		case 5:
			close(nextWait)
		}
		return time.Now()
	}
	e.mu.Unlock()
	var hooks, checks atomic.Int32
	var exhausted atomic.Bool
	var secondProof context.Context
	done := make(chan error, 1)
	t.Cleanup(func() { cancel(); e.Stop(); <-drained; proofs.Wait() })
	started := time.Now()
	go func() {
		defer close(drained)
		for _, batch := range [][]materialBatchItem{items[:3], items[3:]} {
			err := flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, batch, func(materialBatchItem) error {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("fresh check lost healthy parent: %w", err)
				}
				switch checks.Add(1) {
				case 1:
					proofs.Add(1)
					go func() { defer proofs.Done(); _, err := e.Reprove(ctx); secondDone <- err }()
					select {
					case secondProof = <-entered:
					case <-ctx.Done():
						return ctx.Err()
					}
				case 2:
					close(secondCheck)
				case 4:
					close(nextCheck)
				}
				return nil
			}, func(materialBatchItem) error { return errors.New("warm bytes entered repair") }, func(child context.Context) error {
				n := hooks.Add(1)
				if n == 4 {
					exhausted.Store(child.Err() == context.DeadlineExceeded)
				} else {
					capture.Store(n)
				}
				return e.waitForActiveProof(child, 5*time.Second)
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-firstWait:
	case <-time.After(time.Second):
		t.Fatal("first valid proof was not captured")
	}
	// Two genuine renewals make independent per-file waits exceed the old
	// group allowance. The second proof must survive the scheduling cutoff.
	time.Sleep(1100 * time.Millisecond)
	close(firstRelease)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	<-secondWait
	select {
	case <-secondCheck:
	case err := <-done:
		t.Fatalf("group cutoff became sender failure: %v", err)
	case <-time.After(1300 * time.Millisecond):
		t.Fatal("group exceeded the original two-second aggregate wait allowance")
	}
	if elapsed := time.Since(started); elapsed > materialReproofInterval+500*time.Millisecond {
		t.Fatalf("aggregate scheduling elapsed=%v exceeded original allowance", elapsed)
	}
	if firstProof.Err() == nil || secondProof.Err() != nil || !e.Eligible() {
		t.Fatal("scheduling cutoff canceled or invalidated the active valid renewal")
	}
	select {
	case <-nextWait:
	case err := <-done:
		t.Fatalf("next group did not reset its scheduling allowance: %v", err)
	case <-time.After(time.Second):
		t.Fatal("next group never captured the still-active renewal")
	}
	select {
	case <-nextCheck:
		t.Fatal("next group inherited exhausted scheduling allowance")
	case <-time.After(25 * time.Millisecond):
	}
	close(secondRelease)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || checks.Load() != 4 || hooks.Load() != 6 || !exhausted.Load() {
		t.Fatalf("checks=%d hooks=%d exhausted=%v err=%v", checks.Load(), hooks.Load(), exhausted.Load(), err)
	}
	expected := map[string]int{}
	for _, item := range items {
		expected[item.Digest] = 1
	}
	requests := f.takeRequests()
	if len(requests) != 2 || len(requests[0]) != 3 || len(requests[1]) != 1 {
		t.Fatalf("actual full-byte requests changed: %v", requests)
	}
	assertCandidateRemoteSetReads(t, requests, expected)
}

func TestMaterialWarmWaitBudgetRetainsUnrelatedAndJoinedErrors(t *testing.T) {
	problem := errors.New("actual callback failure")
	for name, cause := range map[string]error{
		"unrelated": problem, "cancel": context.Canceled, "healthy-child-deadline": context.DeadlineExceeded,
		"wrapped-deadline": fmt.Errorf("unrelated timeout: %w", context.DeadlineExceeded),
		"joined-deadline":  errors.Join(context.DeadlineExceeded, problem),
	} {
		t.Run(name, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			items := negotiatedMaterialItems(t, f, 2)
			checks := 0
			err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, f.memberID, items,
				func(materialBatchItem) error { checks++; return nil },
				func(materialBatchItem) error { return errors.New("unexpected repair") },
				func(child context.Context) error {
					if child.Err() != nil {
						t.Fatalf("fresh child unexpectedly canceled: %v", child.Err())
					}
					return cause
				})
			if err != cause || checks != 0 {
				t.Fatalf("callback error changed or authorized: checks=%d err=%v want=%v", checks, err, cause)
			}
		})
	}
}

func TestMaterialWarmWaitBudgetExpiredChildDoesNotHideJoinedError(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 2)
	problem := errors.Join(context.DeadlineExceeded, errors.New("observed independent failure"))
	var hooks, checks int
	err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, f.memberID, items,
		func(materialBatchItem) error { checks++; return nil },
		func(materialBatchItem) error { return errors.New("unexpected repair") }, func(child context.Context) error {
			hooks++
			<-child.Done()
			if hooks == 1 {
				return child.Err()
			}
			return problem
		})
	if err != problem || checks != 0 || hooks != 2 || t.Context().Err() != nil {
		t.Fatalf("expired child hid combined failure: checks=%d hooks=%d err=%v", checks, hooks, err)
	}
}

func TestMaterialWarmWaitBudgetParentCancelNeverBecomesSchedulingSuccess(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	err := flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items,
		func(materialBatchItem) error { checks++; return nil },
		func(materialBatchItem) error { return errors.New("unexpected repair") }, func(child context.Context) error {
			cancel()
			<-child.Done()
			return nil
		})
	if !errors.Is(err, context.Canceled) || checks != 0 {
		t.Fatalf("parent cancellation authorized fresh checks: checks=%d err=%v", checks, err)
	}
}
