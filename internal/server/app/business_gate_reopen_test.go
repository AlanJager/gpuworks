//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// reopenGateFixture returns a gate with one installed 200 business handler and
// an eligibility whose background proof loop is driven by the test.
func reopenGateFixture(t *testing.T, interval time.Duration, prove func(context.Context) (bool, error)) (*businessHandlerGate, *materialEligibility, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	return reopenGateFixtureClock(t, nil, interval, prove)
}

func reopenGateFixtureClock(t *testing.T, now func() time.Time, interval time.Duration, prove func(context.Context) (bool, error)) (*businessHandlerGate, *materialEligibility, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	e := newMaterialEligibility(now, prove, interval)
	e.setOnInvalidated(func(admissionCloseCause) {})
	gate.setMaterialEligibility(e)
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("business-ok"))
	}))
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() { defer close(runDone); e.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-runDone })
	return gate, e, cancel, runDone
}

// provenThenClosed models a flap: the member proves once, the next proof
// closes admission, and later proofs follow the test's own behavior.
func provenThenClosed(later func(context.Context) (bool, error)) func(context.Context) (bool, error) {
	var calls atomic.Int32
	return func(ctx context.Context) (bool, error) {
		switch calls.Add(1) {
		case 1:
			return true, nil
		case 2:
			return false, nil
		}
		return later(ctx)
	}
}

// awaitClosedAfterProof waits until the fixture has proven once and then
// closed while its loop still runs. A refresh that merely fails keeps an
// unexpired earlier proof, so the closure is an explicit invalidation, as a
// leadership flap or a membership change makes it.
func awaitClosedAfterProof(t *testing.T, e *materialEligibility) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		e.mu.Lock()
		proved := !e.provedAt.IsZero()
		e.mu.Unlock()
		if proved {
			e.Invalidate(closeMaterialPass)
			if !e.Eligible() {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fixture never proved and then closed")
}

func reopenRequest(gate *businessHandlerGate, ctx context.Context) (<-chan *httptest.ResponseRecorder, <-chan struct{}) {
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil).WithContext(ctx))
	}()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { <-done; result <- rec }()
	return result, done
}

func TestBusinessGateWaitsForTransientReopen(t *testing.T) {
	reopen := make(chan struct{})
	gate, e, _, _ := reopenGateFixture(t, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
		select {
		case <-reopen:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}))
	// A recent proof closed by the next one; the loop is still alive.
	awaitClosedAfterProof(t, e)
	result, done := reopenRequest(gate, t.Context())
	select {
	case <-done:
		t.Fatal("closed gate answered instead of waiting for reopen")
	case <-time.After(150 * time.Millisecond):
	}
	started := time.Now()
	close(reopen)
	select {
	case rec := <-result:
		elapsed := time.Since(started)
		if rec.Code != http.StatusOK || rec.Body.String() != "business-ok" {
			t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
		}
		if elapsed > businessTransientReopenWait {
			t.Fatalf("reopen took %s beyond the wait budget", elapsed)
		}
	case <-time.After(businessTransientReopenWait + 2*time.Second):
		t.Fatal("request never completed after eligibility reopened")
	}
}

func TestBusinessGateReopenBudgetExhausted(t *testing.T) {
	gate, e, _, _ := reopenGateFixture(t, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
		return false, nil
	}))
	awaitClosedAfterProof(t, e)
	if !e.activelyReproofing() {
		t.Fatal("recent closure was not treated as transient")
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil))
	elapsed := time.Since(started)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "member business service is not ready\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if elapsed < businessTransientReopenWait-200*time.Millisecond || elapsed > businessTransientReopenWait+time.Second {
		t.Fatalf("closed member answered in %s, want one reopen budget (%s)", elapsed, businessTransientReopenWait)
	}
}

func TestBusinessGateDefinitiveClosureStaysImmediate(t *testing.T) {
	for _, mode := range []string{"never-started", "stopped", "run-canceled"} {
		t.Run(mode, func(t *testing.T) {
			// Every mode starts from a recent proof, so only the loop's
			// liveness can make the closure definitive.
			gate, e, cancel, runDone := reopenGateFixture(t, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
				return false, nil
			}))
			awaitClosedAfterProof(t, e)
			switch mode {
			case "stopped":
				e.Stop()
			case "run-canceled":
				cancel()
				select {
				case <-runDone:
				case <-time.After(time.Second):
					t.Fatal("proof loop did not return after cancel")
				}
			case "never-started":
				// Replace the loop owner with one that never runs.
				quiet := newMaterialEligibility(nil, func(context.Context) (bool, error) { return false, nil }, time.Second)
				quiet.setOnInvalidated(func(admissionCloseCause) {})
				quiet.mu.Lock()
				quiet.provedAt = quiet.now()
				quiet.mu.Unlock()
				gate.setMaterialEligibility(quiet)
			}
			started := time.Now()
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
			}
			if time.Since(started) > time.Second {
				t.Fatalf("definitive closure held the request %s", time.Since(started))
			}
		})
	}
}

func TestBusinessGateReopenRespectsRequestCancel(t *testing.T) {
	gate, e, _, _ := reopenGateFixture(t, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}))
	awaitClosedAfterProof(t, e)
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	result, done := reopenRequest(gate, requestCtx)
	select {
	case <-done:
		t.Fatal("closed gate answered before the request was canceled")
	case <-time.After(100 * time.Millisecond):
	}
	cancelRequest()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("canceled request outlived its context")
	}
}

func TestBusinessGateReadinessProbeStaysImmediate(t *testing.T) {
	gate, e, _, _ := reopenGateFixture(t, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
		return false, nil
	}))
	awaitClosedAfterProof(t, e)
	started := time.Now()
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || time.Since(started) > time.Second {
		t.Fatalf("probe waited or changed: status=%d elapsed=%s", rec.Code, time.Since(started))
	}
}

// A partitioned minority keeps a live loop that never proves again. Once the
// last proof's lifetime has passed, its closure is definitive: refuse at once.
func TestBusinessGateStaleProofClosureStaysImmediate(t *testing.T) {
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	gate, e, _, _ := reopenGateFixtureClock(t, now, 50*time.Millisecond, provenThenClosed(func(ctx context.Context) (bool, error) {
		return false, nil
	}))
	awaitClosedAfterProof(t, e)
	if !e.activelyReproofing() {
		t.Fatal("closure inside the proof lifetime must stay transient")
	}
	offset.Store(int64(materialEligibilityWindow + time.Second))
	if e.activelyReproofing() {
		t.Fatal("closure past the last proof lifetime still treated as transient")
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "member business service is not ready\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stale closure held the request %s", elapsed)
	}
}

// A live loop that has never proven has no earlier admission to restore.
func TestBusinessGateNeverProvedLiveLoopStaysImmediate(t *testing.T) {
	gate, e, _, _ := reopenGateFixture(t, 50*time.Millisecond, func(ctx context.Context) (bool, error) {
		return false, nil
	})
	for i := 0; i < 200; i++ {
		e.mu.Lock()
		running := e.running
		e.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if !running {
		t.Fatal("fixture loop never started")
	}
	if e.activelyReproofing() {
		t.Fatal("never-proved loop treated as transient")
	}
	started := time.Now()
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("never-proved closure held the request %s", elapsed)
	}
}
