//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The controlled prover tests scheduling only. The batches still use the
// production authenticated TLS handler; native cases verify grant authority.
func warmYieldActiveProof(t *testing.T) (*materialEligibility, chan struct{}, <-chan error) {
	return warmYieldProofResult(t, true, nil)
}

func warmYieldProofResult(t *testing.T, result bool, failure error) (*materialEligibility, chan struct{}, <-chan error) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		close(entered)
		select {
		case <-release:
			return result, failure
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}, materialReproofInterval)
	if ready, err := e.Reprove(t.Context()); !ready || err != nil {
		t.Fatalf("initial scheduling result ready=%v err=%v", ready, err)
	}
	done := make(chan error, 1)
	go func() { _, err := e.Reprove(t.Context()); done <- err }()
	<-entered
	t.Cleanup(e.Stop)
	return e, release, done
}

func warmYieldLaterProof(t *testing.T, ctx context.Context) (*materialEligibility, chan context.Context, chan struct{}) {
	t.Helper()
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var calls atomic.Int32
	e := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		entered <- ctx
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}, materialReproofInterval)
	if ready, err := e.Reprove(ctx); !ready || err != nil {
		t.Fatalf("initial scheduling result ready=%v err=%v", ready, err)
	}
	t.Cleanup(e.Stop)
	return e, entered, release
}

func TestMaterialWarmBatchYieldRenewalStartingAfterFirstCheck(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 33)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e, proofEntered, release := warmYieldLaterProof(t, ctx)
	proofDone := make(chan error, 1)
	secondYield, secondCheck, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	proofDrained := make(chan struct{})
	var capture atomic.Bool
	e.now = func() time.Time {
		if capture.CompareAndSwap(true, false) {
			close(secondYield)
		}
		return time.Now()
	}
	done := make(chan error, 1)
	var yields, checks atomic.Int32
	t.Cleanup(func() {
		cancel()
		e.Stop()
		<-drained
		if checks.Load() > 0 {
			<-proofDrained
		}
	})
	go func() {
		defer close(drained)
		for _, batch := range [][]materialBatchItem{items[:32], items[32:]} {
			err := flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, batch, func(materialBatchItem) error {
				switch checks.Add(1) {
				case 1:
					go func() { defer close(proofDrained); _, err := e.Reprove(ctx); proofDone <- err }()
					select {
					case <-proofEntered:
					case <-ctx.Done():
						return ctx.Err()
					}
				case 2:
					close(secondCheck)
				}
				return nil
			}, func(materialBatchItem) error {
				return errors.New("verified warm batch entered repair")
			}, func(ctx context.Context) error {
				// Pre-hint, first-file, then the second-file boundary.
				if yields.Add(1) == 3 {
					capture.Store(true)
				}
				return e.waitForActiveProof(ctx, 5*time.Second)
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-secondCheck:
		t.Fatal("second fresh check bypassed renewal started after first check")
	case <-secondYield:
	case err := <-done:
		t.Fatalf("warm sender finished before second scheduling boundary: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second scheduling boundary was not reached")
	}
	select {
	case <-secondCheck:
		t.Fatal("second fresh check did not wait for captured renewal")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-proofDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || checks.Load() != 33 || yields.Load() != 35 {
		t.Fatalf("warm checks=%d yields=%d err=%v", checks.Load(), yields.Load(), err)
	}
	expected := map[string]int{}
	for _, item := range items {
		expected[item.Digest] = 1
	}
	requests := f.takeRequests()
	if len(requests) != 2 || len(requests[0]) != 32 || len(requests[1]) != 1 {
		t.Fatalf("warm batch grouping changed: %v", requests)
	}
	assertCandidateRemoteSetReads(t, requests, expected)
}

func TestMaterialWarmBatchYieldChecksBeforeEveryFreshGrant(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := negotiatedMaterialItems(t, f, 33)
	e, release, proofDone := warmYieldActiveProof(t)
	var yields, checks atomic.Int32
	entered := make(chan struct{}, 1)
	firstCheck := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		for _, batch := range [][]materialBatchItem{items[:32], items[32:]} {
			err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, f.memberID, batch, func(materialBatchItem) error {
				checks.Add(1)
				select {
				case firstCheck <- struct{}{}:
				default:
				}
				return nil
			}, func(materialBatchItem) error {
				t.Error("complete warm bytes entered repair")
				return nil
			}, func(ctx context.Context) error {
				yields.Add(1)
				select {
				case entered <- struct{}{}:
				default:
				}
				return e.waitForActiveProof(ctx, 5*time.Second)
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-firstCheck:
		t.Fatal("sender file check preceded the active valid proof")
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("verified batch did not reach the scheduling boundary")
	}
	select {
	case <-firstCheck:
		t.Fatal("sender file check bypassed the active proof")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-proofDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || checks.Load() != 33 || yields.Load() != 35 {
		t.Fatalf("warm checks=%d yields=%d err=%v", checks.Load(), yields.Load(), err)
	}
	requests := f.takeRequests()
	if len(requests) != 2 || len(requests[0]) != 32 || len(requests[1]) != 1 {
		t.Fatalf("warm batch grouping changed: %v", requests)
	}
	expected := map[string]int{}
	for _, item := range items {
		expected[item.Digest] = 1
	}
	assertCandidateRemoteSetReads(t, requests, expected)
}

func TestMaterialWarmBatchYieldStopsAtCapturedProof(t *testing.T) {
	for _, mode := range []string{"caller-cancel", "invalidate", "stop", "proof-failure"} {
		t.Run(mode, func(t *testing.T) {
			var e *materialEligibility
			var release chan struct{}
			var proofDone <-chan error
			if mode == "proof-failure" {
				e, release, proofDone = warmYieldProofResult(t, false, errors.New("real controlled proof failure"))
			} else {
				e, release, proofDone = warmYieldActiveProof(t)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- e.waitForActiveProof(ctx, 5*time.Second) }()
			select {
			case err := <-done:
				t.Fatalf("active proof skipped: %v", err)
			case <-time.After(15 * time.Millisecond):
			}
			switch mode {
			case "caller-cancel":
				cancel()
			case "invalidate":
				e.Invalidate(closeMaterialPass)
			case "stop":
				e.Stop()
			case "proof-failure":
				close(release)
				if err := <-proofDone; err == nil {
					t.Fatal("failed proof unexpectedly succeeded")
				}

			}
			select {
			case err := <-done:
				if mode == "caller-cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("caller cancellation lost: %v", err)
					}
				} else if err != nil {
					t.Fatalf("proof cancellation became sender error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("captured proof completion did not release sender")
			}
			if mode != "proof-failure" {
				e.Stop()
				<-proofDone
			}
		})
	}
}

func TestMaterialWarmBatchYieldBypassesUnprovenOrInactiveProof(t *testing.T) {
	for _, mode := range []string{"nil", "cold", "expired", "stopped", "no-proof", "proof-canceled", "run-canceled", "exact-headroom"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			proof, cancelProof := context.WithCancel(t.Context())
			defer cancelProof()
			e := newMaterialEligibility(func() time.Time { return now }, nil, materialReproofInterval)
			<-e.proofSlot
			slot := e.proofSlot
			defer func() { slot <- struct{}{} }()
			e.deadline = now.Add(10 * time.Second)
			e.proofCtx = proof
			switch mode {
			case "nil":
				e = nil
			case "cold":
				e.deadline = time.Time{}
			case "expired":
				e.deadline = now
			case "stopped":
				e.stopped = true
			case "no-proof":
				e.proofCtx = nil
			case "proof-canceled":
				cancelProof()
			case "run-canceled":
				c, stop := context.WithCancel(t.Context())
				stop()
				e.runCtx = c
			case "exact-headroom":
				e.deadline = now.Add(5 * time.Second)
			}
			done := make(chan error, 1)
			go func() { done <- e.waitForActiveProof(t.Context(), 5*time.Second) }()
			if mode == "exact-headroom" {
				select {
				case err := <-done:
					t.Fatalf("exact threshold skipped proof: %v", err)
				case <-time.After(15 * time.Millisecond):
				}
				cancelProof()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("bypass blocked")
			}
		})
	}
}

// A snapshot barrier ensures the caller actually enters waitForActiveProof.
// These cases test scheduling only; native cases use real membership grants.
func warmYieldSnapshot(t *testing.T) (*materialEligibility, context.CancelFunc, <-chan struct{}, chan struct{}) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var observed atomic.Bool
	now := time.Now()
	e := newMaterialEligibility(func() time.Time {
		if observed.CompareAndSwap(false, true) {
			close(entered)
			<-resume
		}
		return now
	}, nil, materialReproofInterval)
	proof, cancel := context.WithCancel(t.Context())
	e.deadline = now.Add(10 * time.Second)
	e.proofCtx = proof
	<-e.proofSlot
	return e, cancel, entered, resume
}

func TestMaterialWarmBatchYieldDoesNotChaseReplacement(t *testing.T) {
	e, finishOld, entered, resume := warmYieldSnapshot(t)
	defer finishOld()
	done := make(chan error, 1)
	go func() { done <- e.waitForActiveProof(t.Context(), 5*time.Second) }()
	<-entered
	close(resume)
	replacement, stop := context.WithCancel(t.Context())
	defer stop()
	e.mu.Lock()
	e.proofCtx = replacement
	e.mu.Unlock()
	finishOld()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sender chased the replacement proof")
	}
	// Replacement still owns the token and is live throughout the assertion.
	if replacement.Err() != nil || len(e.proofSlot) != 0 {
		t.Fatal("replacement was not held")
	}
	e.proofSlot <- struct{}{}
}

func TestMaterialWarmBatchYieldReturnsSlotBeforeFreshCheck(t *testing.T) {
	e, finishOld, entered, resume := warmYieldSnapshot(t)
	defer finishOld()
	done := make(chan error, 1)
	go func() { done <- e.waitForActiveProof(t.Context(), 5*time.Second) }()
	<-entered
	close(resume)
	// Synchronize past the snapshot. The old proof stays live, so only receipt
	// of this newly supplied token can end the wait.
	e.mu.Lock()
	old := e.proofCtx
	e.mu.Unlock()
	e.proofSlot <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("available slot did not release sender")
	}
	if old.Err() != nil {
		t.Fatal("old proof cancellation hid token path")
	}
	select {
	case <-e.proofSlot:
		e.proofSlot <- struct{}{}
	case <-time.After(time.Second):
		t.Fatal("warm waiter retained proof slot")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.waitForActiveProof(ctx, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled sender lost error: %v", err)
	}
}

func TestMaterialWarmBatchYieldFailedHintsYieldBeforeEachFallback(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "wrong-member", "wrong-meta", "partial", "grant-503"} {
		t.Run(mode, func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			items := negotiatedMaterialItems(t, f, 2)
			member := f.memberID
			name := filepath.Join(f.root, items[0].Digest[:2], items[0].Digest)
			switch mode {
			case "missing":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(name, []byte("wrong"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-member":
				member++
			case "wrong-meta", "partial", "grant-503":
				f.mu.Lock()
				f.beforeServe = func(w http.ResponseWriter, r *http.Request) bool {
					if mode == "grant-503" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return true
					}
					// Generate the complete authenticated production response first, then
					// change only this network response. Partial bodies have valid metadata
					// and full Content-Length, so body verification itself must reject them.
					recorded := httptest.NewRecorder()
					f.handler.ServeHTTP(recorded, r)
					if recorded.Code != http.StatusOK {
						t.Errorf("real batch response=%d", recorded.Code)
					}
					for key, values := range recorded.Header() {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					if mode == "wrong-meta" {
						w.Header().Set("X-Gpuworks-Material-Member", "999")
					}
					w.WriteHeader(recorded.Code)
					body := recorded.Body.Bytes()
					if mode == "partial" {
						body = body[:len(body)-1]
					}
					_, _ = w.Write(body)
					return true
				}
				f.mu.Unlock()
			}
			var checks, repairs, yields int
			err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, member, items, func(materialBatchItem) error { checks++; return nil }, func(materialBatchItem) error { repairs++; return nil }, func(context.Context) error { yields++; return nil })
			if err != nil || checks != 0 || repairs != 2 || yields != 3 {
				t.Fatalf("failed full batch err=%v checks=%d repairs=%d yields=%d", err, checks, repairs, yields)
			}
		})
	}
}

func TestMaterialWarmBatchYieldCallerErrorPreventsFreshChecks(t *testing.T) {
	for _, callbackReturnsError := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled-nil-result", true: "canceled-error-result"}[callbackReturnsError], func(t *testing.T) {
			f := newCandidateRemoteSetFixture(t, 7)
			items := negotiatedMaterialItems(t, f, 2)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var checks, repairs, yields int
			err := flushMaterialReceiptHintsWithYield(ctx, f.client, f.server.URL, f.memberID, items,
				func(materialBatchItem) error { checks++; return nil },
				func(materialBatchItem) error { repairs++; return nil }, func(ctx context.Context) error {
					yields++
					cancel()
					if callbackReturnsError {
						return ctx.Err()
					}
					return nil
				})
			if !errors.Is(err, context.Canceled) || checks != 0 || repairs != 0 || yields != 1 {
				t.Fatalf("cancellation err=%v checks=%d repairs=%d yields=%d", err, checks, repairs, yields)
			}
		})
	}
}

func TestMaterialWarmBatchYieldBoundsSlowProofAndShortHeadroom(t *testing.T) {
	for _, mode := range []string{"existing-cadence", "short-valid-deadline", "operation-cap"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			proof, finish := context.WithCancel(t.Context())
			defer finish()
			e := newMaterialEligibility(time.Now, nil, materialReproofInterval)
			e.deadline = now.Add(10 * time.Second)
			e.proofCtx = proof
			<-e.proofSlot
			defer func() { e.proofSlot <- struct{}{} }()
			cap := 5 * time.Second
			expected := materialReproofInterval
			switch mode {
			case "short-valid-deadline":
				e.deadline = now.Add(150 * time.Millisecond)
				expected = 150 * time.Millisecond
			case "operation-cap":
				cap = 150 * time.Millisecond
				expected = cap
			}
			ctx, cancel := context.WithTimeout(t.Context(), expected+750*time.Millisecond)
			defer cancel()
			deadline, epoch := e.deadline, e.epoch
			started := time.Now()
			err := e.waitForActiveProof(ctx, cap)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("schedule wait did not finish within existing bound: elapsed=%v err=%v", elapsed, err)
			}
			if elapsed < expected-50*time.Millisecond {
				t.Fatalf("valid active proof bypassed bounded warm wait: elapsed=%v expected=%v", elapsed, expected)
			}
			if proof.Err() != nil || len(e.proofSlot) != 0 {
				t.Fatal("elapsed scheduling bound canceled proof or changed slot ownership")
			}
			if ctx.Err() != nil {
				t.Fatal("scheduling bound consumed caller deadline")
			}
			if e.deadline != deadline || e.epoch != epoch {
				t.Fatal("scheduling wait changed proof authority")
			}
		})
	}
}

func TestMaterialWarmBatchYieldAtFullByteLimit(t *testing.T) {
	f := newCandidateRemoteSetFixture(t, 7)
	items := []materialBatchItem{f.put(bytes.Repeat([]byte("a"), 4<<20)), f.put(bytes.Repeat([]byte("b"), 4<<20))}
	var checks, repairs, yields int
	check := func(materialBatchItem) error { checks++; return nil }
	repair := func(materialBatchItem) error { repairs++; return nil }
	yield := func(context.Context) error { yields++; return nil }
	if err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, f.memberID, items, check, repair, yield); err != nil {
		t.Fatal(err)
	}
	if checks != 2 || repairs != 0 || yields != 3 {
		t.Fatalf("full byte-limit batch checks=%d repairs=%d yields=%d", checks, repairs, yields)
	}
	requests := f.takeRequests()
	if len(requests) != 1 {
		t.Fatalf("byte-limit full body requests=%d", len(requests))
	}
	assertCandidateRemoteSetReads(t, requests, map[string]int{items[0].Digest: 1, items[1].Digest: 1})
	checks, repairs, yields = 0, 0, 0
	items = append(items, f.put([]byte("x")))
	if err := flushMaterialReceiptHintsWithYield(t.Context(), f.client, f.server.URL, f.memberID, items, check, repair, yield); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || repairs != 3 || yields != 4 {
		t.Fatalf("oversized whole hint batch skipped fallback or scheduling: checks=%d repairs=%d yields=%d", checks, repairs, yields)
	}
	if len(f.takeRequests()) != 0 {
		t.Fatal("oversized batch reached remote")
	}
}
