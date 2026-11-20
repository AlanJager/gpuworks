//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// This starts an actual Reprove whose result is withheld until release. The
// optional earlier proof expires through its clock, without fabricating a grant.
func pendingWorkerProof(t *testing.T, expired, ready bool) (*materialEligibility, func(), <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	resume := sync.OnceFunc(func() { close(release) })
	entered := make(chan struct{})
	var calls atomic.Int32
	now := time.Now()
	e := newMaterialEligibility(func() time.Time { return now }, func(ctx context.Context) (bool, error) {
		if expired && calls.Add(1) == 1 {
			return true, nil
		}
		close(entered)
		select {
		case <-release:
			return ready, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}, materialReproofInterval)
	if expired {
		if ready, err := e.Reprove(ctx); !ready || err != nil {
			t.Fatalf("initial proof: ready=%t err=%v", ready, err)
		}
		now = now.Add(materialEligibilityWindow)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = e.Reprove(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("actual pending proof did not start")
	}
	t.Cleanup(func() { cancel(); resume(); e.Stop(); <-done })
	return e, resume, done
}

func TestWorkerProofWaitYieldsWithZeroOrExpiredResult(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "zero", true: "expired"}[expired], func(t *testing.T) {
			e, release, proofDone := pendingWorkerProof(t, expired, true)
			a := &App{memberState: memberState{materialEligibility: e}}
			// An active renewal may retain ordinary admission from an earlier
			// proof. It cannot provide the headroom for a new bounded grant.
			if e.Eligible() != expired || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("pending proof changed ordinary admission or granted fresh headroom")
			}
			e.mu.Lock()
			deadline, epoch := e.deadline, e.epoch
			e.mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- a.yieldWarmMaterialBatch(t.Context()) }()
			select {
			case err := <-done:
				t.Fatalf("worker bypassed the current proof with no live old result: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			e.mu.Lock()
			unchanged := e.deadline.Equal(deadline) && e.epoch == epoch
			e.mu.Unlock()
			if !unchanged || e.Eligible() != expired || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("worker scheduling changed the proof deadline, epoch or grant headroom")
			}
			release()
			<-proofDone
			if err := <-done; err != nil || !e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatalf("worker did not resume after genuine Reprove: headroom=%t err=%v", e.EligibleFor(store.ClusterMembershipGrantTimeout), err)
			}
		})
	}
}

func TestWorkerProofWaitBoundedWithoutChangingReadiness(t *testing.T) {
	e, _, _ := pendingWorkerProof(t, false, true)
	started := time.Now()
	if err := e.waitForWorkerProof(t.Context(), 45*time.Millisecond); err != nil || time.Since(started) < 45*time.Millisecond {
		t.Fatalf("worker scheduling cutoff was skipped: elapsed=%v err=%v", time.Since(started), err)
	}
	if e.Eligible() {
		t.Fatal("worker cutoff opened authority")
	}
	done := make(chan error, 1)
	go func() { done <- e.waitForActiveProof(t.Context(), time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Millisecond):
		t.Fatal("worker scheduling changed the request-path zero-result wait")
	}
}

func TestWorkerProofWaitCancellationDoesNotCancelCurrentProof(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "zero", true: "expired"}[expired], func(t *testing.T) {
			e, _, _ := pendingWorkerProof(t, expired, true)
			e.mu.Lock()
			originalDeadline, originalEpoch := e.deadline, e.epoch
			e.mu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- e.waitForWorkerProof(ctx, time.Second) }()
			select {
			case err := <-done:
				t.Fatalf("worker skipped its pending wait before cancellation: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled worker wait lost its cause: %v", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("active worker wait did not immediately honor cancellation")
			}
			e.mu.Lock()
			proofCtx := e.proofCtx
			unchanged := e.deadline.Equal(originalDeadline) && e.epoch == originalEpoch
			e.mu.Unlock()
			if proofCtx == nil || proofCtx.Err() != nil || !unchanged || e.Eligible() != expired || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("worker cancellation canceled, refreshed or authorized the genuine proof")
			}
		})
	}
}

func TestWorkerProofWaitSkipsUnavailableOwners(t *testing.T) {
	for _, state := range []string{"nil", "no-active", "stopped", "run-canceled", "proof-canceled"} {
		t.Run(state, func(t *testing.T) {
			var e *materialEligibility
			switch state {
			case "no-active":
				e = newMaterialEligibility(time.Now, nil, materialReproofInterval)
			case "stopped", "run-canceled", "proof-canceled":
				e, _, _ = pendingWorkerProof(t, false, true)
				switch state {
				case "stopped":
					e.Stop()
				case "run-canceled":
					owner, cancel := context.WithCancel(t.Context())
					cancel()
					e.mu.Lock()
					e.runCtx = owner
					e.mu.Unlock()
				case "proof-canceled":
					e.mu.Lock()
					cancel := e.cancel
					e.mu.Unlock()
					cancel()
				}
			}
			done := make(chan error, 1)
			go func() { done <- e.waitForWorkerProof(t.Context(), time.Second) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(40 * time.Millisecond):
				t.Fatal("worker waited on an unavailable proof owner")
			}
		})
	}
}

func TestWarmFailedBatchYieldsEveryFallbackWithinSharedBudget(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 3)
	path := filepath.Join(f.root, items[1].Digest[:2], items[1].Digest)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(original)), 0600); err != nil {
		t.Fatal(err)
	}
	e, _, _ := pendingWorkerProof(t, false, true)
	ctx, cancel := context.WithTimeout(t.Context(), materialReproofInterval+time.Second)
	defer cancel()
	hooks, checks, repairs, expiredChildren := 0, 0, 0, 0
	started := time.Now()
	err = flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items,
		func(materialBatchItem) error { checks++; return errors.New("corrupt group was treated as verified") },
		func(file materialBatchItem) error {
			repairs++
			return verifyRemoteMaterialReplica(ctx, f.client, f.server.URL, file.Digest, file.Bytes, f.memberID)
		}, func(child context.Context) error {
			hooks++
			waitErr := e.waitForWorkerProof(child, 5*time.Second)
			if child.Err() == context.DeadlineExceeded {
				expiredChildren++
			}
			// Repair only after the corrupt hint has failed; pre-hint yielding
			// must not turn this negative full-byte response into success.
			if hooks == 2 {
				if err := os.WriteFile(path, original, 0600); err != nil {
					return err
				}
			}
			return waitErr
		})
	if err != nil || hooks != len(items)+1 || repairs != len(items) || checks != 0 || expiredChildren == 0 {
		t.Fatalf("failed group bypassed yield or original fallback: hooks=%d repairs=%d checks=%d expired=%d err=%v", hooks, repairs, checks, expiredChildren, err)
	}
	if elapsed := time.Since(started); elapsed < materialReproofInterval || elapsed >= materialReproofInterval+time.Second {
		t.Fatalf("fallback waits escaped the shared group allowance: %v", elapsed)
	}
	if e.Eligible() {
		t.Fatal("fallback scheduling admitted an unfinished proof")
	}
	requests := f.takeRequests()
	if len(requests) != 1+len(items) || len(requests[0]) != len(items) {
		t.Fatalf("failed full-byte batch did not retain every real single-file read: %v", requests)
	}
	for i, file := range items {
		if len(requests[i+1]) != 1 || requests[i+1][0].Digest != file.Digest {
			t.Fatalf("fallback file%d was skipped: %v", i, requests[i+1])
		}
	}
}

func TestWarmFailedBatchRetainsCancellationAndJoinedWaitErrors(t *testing.T) {
	for _, fault := range []string{"parent-cancel", "joined-deadline", "expired-joined-deadline"} {
		t.Run(fault, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			items := negotiatedMaterialItems(t, f, 2)
			path := filepath.Join(f.root, items[0].Digest[:2], items[0].Digest)
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(items[0].Bytes)), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.Join(context.DeadlineExceeded, errors.New("independent scheduling failure"))
			checks, repairs, hooks := 0, 0, 0
			err := flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items,
				func(materialBatchItem) error { checks++; return nil },
				func(materialBatchItem) error { repairs++; return nil },
				func(child context.Context) error {
					hooks++
					if fault == "parent-cancel" {
						cancel()
						return nil
					}
					if fault == "expired-joined-deadline" {
						<-child.Done()
					}
					return cause
				})
			if (fault == "parent-cancel" && !errors.Is(err, context.Canceled)) || (fault != "parent-cancel" && err != cause) || hooks != 1 || checks != 0 || repairs != 0 {
				t.Fatalf("failed batch changed rejection rules: hooks=%d checks=%d repairs=%d err=%v", hooks, checks, repairs, err)
			}
		})
	}
}

func TestWorkerProofWaitCannotResurrectIdleExpiredResult(t *testing.T) {
	now := time.Now()
	var calls int
	e := newMaterialEligibility(func() time.Time { return now }, func(context.Context) (bool, error) {
		calls++
		return true, nil
	}, materialReproofInterval)
	t.Cleanup(e.Stop)
	if ready, err := e.Reprove(t.Context()); !ready || err != nil {
		t.Fatalf("initial proof: %t %v", ready, err)
	}
	now = now.Add(materialEligibilityWindow)
	e.mu.Lock()
	deadline, epoch := e.deadline, e.epoch
	e.mu.Unlock()
	if err := e.waitForWorkerProof(t.Context(), 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	unchanged := e.deadline.Equal(deadline) && e.epoch == epoch
	e.mu.Unlock()
	if !unchanged || calls != 1 || e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
		t.Fatal("idle worker wait changed or revived the expired proof")
	}
}

func TestWorkerProofWaitRetainsExpiredRenewalRejectionFences(t *testing.T) {
	for _, event := range []string{"false-nil", "invalidate", "cancel"} {
		t.Run(event, func(t *testing.T) {
			e, release, proofDone := pendingWorkerProof(t, true, false)
			if !e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("invalid initial ordinary/grant boundary")
			}
			waiter := make(chan error, 1)
			go func() { waiter <- e.waitForWorkerProof(t.Context(), time.Second) }()
			select {
			case err := <-waiter:
				t.Fatalf("worker bypassed the still-pending renewal: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			switch event {
			case "false-nil":
				release()
			case "invalidate":
				e.Invalidate()
			case "cancel":
				e.mu.Lock()
				cancel := e.cancel
				e.mu.Unlock()
				cancel()
			}
			if event != "false-nil" && (e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout)) {
				t.Fatal("explicit loss retained ordinary or fresh authority")
			}
			select {
			case err := <-waiter:
				if err != nil {
					t.Fatalf("proof rejection became a caller cancellation: %v", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("rejected proof did not wake its active worker waiter")
			}
			<-proofDone
			if e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("rejected renewal retained authority")
			}
			e.mu.Lock()
			deadline, epoch := e.deadline, e.epoch
			e.mu.Unlock()
			if err := e.waitForWorkerProof(t.Context(), 40*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			e.mu.Lock()
			unchanged := e.deadline.Equal(deadline) && e.epoch == epoch
			e.mu.Unlock()
			if !unchanged || e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatal("worker wait rescued a failed, invalidated or canceled proof")
			}
		})
	}
}
