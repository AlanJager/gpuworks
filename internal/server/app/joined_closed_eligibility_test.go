//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type materialEligibilityTestLease struct{ closed atomic.Bool }

type blockingBusinessStop struct {
	once     sync.Once
	stopping atomic.Bool
	entered  chan struct{}
	release  chan struct{}
}

func (g *blockingBusinessStop) BeginStop() {
	g.once.Do(func() {
		g.stopping.Store(true)
		close(g.entered)
		<-g.release
	})
}

func (g *blockingBusinessStop) Stopping() bool             { return g.stopping.Load() }
func (g *blockingBusinessStop) Wait(context.Context) error { return nil }

func (l *materialEligibilityTestLease) Close() error {
	l.closed.Store(true)
	return nil
}

func TestCandidateMaterialInvalidationClosesRequestsBeforeBusinessStopWait(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial material proof: ready=%t err=%v", ready, err)
	}
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	owner := &blockingBusinessStop{entered: make(chan struct{}), release: make(chan struct{})}
	a := &App{memberState: memberState{materialEligibility: eligibility, businessHTTP: gate}}
	if err := a.retainClosedBusiness(owner); err != nil {
		t.Fatal(err)
	}
	eligibility.setOnInvalidated(a.closeBusinessAdmission)
	initial := httptest.NewRecorder()
	gate.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if initial.Code != http.StatusNoContent {
		t.Fatalf("proved business route status=%d", initial.Code)
	}
	done := make(chan struct{})
	go func() {
		a.invalidateCandidateMaterialEligibility()
		close(done)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(owner.release) }) }
	defer release()
	select {
	case <-owner.entered:
	case <-time.After(time.Second):
		t.Fatal("business generation did not enter blocking stop")
	}
	if eligibility.Eligible() {
		t.Fatal("business stop wait retained old material eligibility")
	}
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("request entered business while old generation stop was blocked: %d", response.Code)
	}
	release()
	<-done
}

func TestClosedMaterialEligibilityCloseCancelsAndJoinsProof(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	db, err := store.OpenRuntime(t.Context(), filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	lease := &materialEligibilityTestLease{}
	eligibility := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release // prove Close does not release the Store before the worker returns
		return true, nil
	}, time.Hour)
	gate := newBusinessHandlerGate()
	a := &App{store: db, ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility, businessHTTP: gate, joinedRaft: lease}}
	if eligibility.Eligible() {
		t.Fatal("new closed App was eligible before proof")
	}
	if err := a.startClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-started
	closed := make(chan error, 1)
	go func() { closed <- a.closeWithEligibilityTimeout(time.Millisecond) }()
	<-canceled
	if err := <-closed; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded Close = %v, want deadline exceeded", err)
	}
	if lease.closed.Load() {
		t.Fatal("timed out Close released runtime lease")
	}
	var one int
	if err := db.DB().Raw("SELECT 1").Scan(&one).Error; err != nil || one != 1 {
		t.Fatalf("timed out Close released Store: value=%d err=%v", one, err)
	}
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed App opened business route: %d", response.Code)
	}
	releaseOnce.Do(func() { close(release) })
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !lease.closed.Load() {
		t.Fatal("retried Close did not release runtime lease")
	}
	if eligibility.Eligible() {
		t.Fatal("late proof reopened stopped App")
	}
	if err := a.startClosedMaterialEligibility(t.Context()); err == nil {
		t.Fatal("closed App started another proof")
	}
}

func TestClosedMaterialEligibilityInvalidationAndRepeatedRunOwner(t *testing.T) {
	first := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: first}}
	ctx, cancel := context.WithCancel(t.Context())
	if err := a.startClosedMaterialEligibility(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for !first.Eligible() {
		select {
		case <-deadline:
			t.Fatal("first run did not prove")
		case <-time.After(time.Millisecond):
		}
	}
	a.invalidateCandidateMaterialEligibility()
	if first.Eligible() {
		t.Fatal("observed change retained old proof")
	}
	cancel()
	if err := a.stopClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	if first.Eligible() {
		t.Fatal("first run retained eligibility after stop")
	}
	second := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a.materialEligibilityMu.Lock()
	a.materialEligibility = second
	a.materialEligibilityMu.Unlock()
	if err := a.startClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.stopClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.Eligible() || first.Eligible() {
		t.Fatal("stopped proof survived a repeated run")
	}
}

func TestClosedMemberBusinessWritesRequireExplicitAdmission(t *testing.T) {
	st, err := store.OpenRuntime(t.Context(), filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a := &App{store: st, ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if err := a.installClosedBusinessWriteBarrier(); err != nil {
		t.Fatal(err)
	}
	create, firstAdmitted := countedFencedWrites(t, st)
	if err := create("before-proof"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("closed member wrote before proof: %v", err)
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("candidate proof: ready=%t err=%v", ready, err)
	}
	if err := create("proof-without-owner"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("proof opened business writes without owner: %v", err)
	}
	if err := st.OpenBusinessWrites(); err != nil {
		t.Fatal(err)
	}
	if err := create("admitted"); err != nil {
		t.Fatal(err)
	}
	a.invalidateCandidateMaterialEligibility()
	if err := create("invalidated"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("material invalidation left writes open: %v", err)
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("renewed candidate proof: ready=%t err=%v", ready, err)
	}
	if err := create("renewed-without-owner"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("renewal reopened writes without owner: %v", err)
	}
	if err := st.OpenBusinessWrites(); err != nil {
		t.Fatal(err)
	}
	if err := a.stopClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a.materialEligibilityMu.Lock()
	a.materialEligibility = next
	a.materialEligibilityMu.Unlock()
	if ready, err := next.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("next Run proof: ready=%t err=%v", ready, err)
	}
	if err := create("next-run-without-owner"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("new proof bypassed previous Run shutdown: %v", err)
	}
	// "admitted" is the third write; no refused write may have landed first.
	if got := firstAdmitted(); got != 3 {
		t.Fatalf("first landed write was call %d, want the one admitted write", got)
	}
}

func TestClosedMemberProofFailureAndExpiryLatchBusinessWritesClosed(t *testing.T) {
	st, err := store.OpenRuntime(t.Context(), filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clock := &materialTestClock{now: time.Unix(100, 0)}
	ready := true
	eligibility := newMaterialEligibility(clock.Now, func(context.Context) (bool, error) { return ready, nil }, time.Hour)
	a := &App{store: st, ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if err := a.installClosedBusinessWriteBarrier(); err != nil {
		t.Fatal(err)
	}
	create, firstAdmitted := countedFencedWrites(t, st)
	if proven, err := eligibility.Reprove(t.Context()); err != nil || !proven {
		t.Fatalf("initial proof: proven=%t err=%v", proven, err)
	}
	if err := st.OpenBusinessWrites(); err != nil {
		t.Fatal(err)
	}
	if err := create("initial"); err != nil {
		t.Fatal(err)
	}
	// A refresh that fails inside the window keeps the earlier proof; the
	// failure closes admission once that proof has run out.
	ready = false
	if proven, err := eligibility.Reprove(t.Context()); err != nil || proven || !eligibility.Eligible() {
		t.Fatalf("failed refresh inside the window: proven=%t eligible=%t err=%v", proven, eligibility.Eligible(), err)
	}
	clock.Advance(materialEligibilityWindow)
	if proven, err := eligibility.Reprove(t.Context()); err != nil || proven || eligibility.Eligible() {
		t.Fatalf("failed proof after the window: proven=%t eligible=%t err=%v", proven, eligibility.Eligible(), err)
	}
	ready = true
	if proven, err := eligibility.Reprove(t.Context()); err != nil || !proven {
		t.Fatalf("recovery proof: proven=%t err=%v", proven, err)
	}
	if err := create("after-failure"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("successful reproof reopened writes after failure: %v", err)
	}
	if err := st.OpenBusinessWrites(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(materialEligibilityWindow)
	if eligibility.Eligible() {
		t.Fatal("expired proof remained eligible")
	}
	if proven, err := eligibility.Reprove(t.Context()); err != nil || !proven {
		t.Fatalf("renewed proof: proven=%t err=%v", proven, err)
	}
	if err := create("after-expiry"); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("successful reproof reopened writes after expiry: %v", err)
	}
	if got := firstAdmitted(); got != 1 {
		t.Fatalf("first landed write was call %d, want only the initial write", got)
	}
}

func TestClosedTransportRefreshDeadlineKeepsCurrentProof(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("initial proof: ready=%v eligible=%v err=%v", ready, eligibility.Eligible(), err)
	}
	err := a.closedTransportAuthorizationRefresh(func(context.Context) error { return context.DeadlineExceeded })(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || !eligibility.Eligible() {
		t.Fatalf("timed out refresh closed admission: err=%v eligible=%v", err, eligibility.Eligible())
	}
}

// pendingTransportDeadlineContext models a socket deadline being observed
// before context's timer goroutine has published cancellation. Err and Done
// still come from the live parent; only the already-reached deadline differs.
type pendingTransportDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c pendingTransportDeadlineContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func TestClosedTransportRefreshBadConnectionContext(t *testing.T) {
	integrityFailure := errors.New("transport identity differs")
	for _, tc := range []struct {
		name          string
		deadline      bool
		clockDeadline time.Duration
		canceled      bool
		failure       error
		keep          bool
	}{
		{name: "expired_attempt_bad_connection", deadline: true, failure: driver.ErrBadConn, keep: true},
		{name: "live_attempt_bad_connection", failure: driver.ErrBadConn, keep: true},
		{name: "elapsed_deadline_pending_context_bad_connection", clockDeadline: -time.Millisecond, failure: driver.ErrBadConn, keep: true},
		{name: "future_deadline_bad_connection", clockDeadline: time.Minute, failure: driver.ErrBadConn, keep: true},
		{name: "elapsed_deadline_parent_canceled_bad_connection", clockDeadline: -time.Millisecond, canceled: true, failure: driver.ErrBadConn},
		{name: "elapsed_deadline_pending_context_integrity_failure", clockDeadline: -time.Millisecond, failure: integrityFailure},
		{name: "parent_canceled_bad_connection", canceled: true, failure: driver.ErrBadConn},
		{name: "parent_canceled_deadline_error", canceled: true, failure: context.DeadlineExceeded},
		{name: "expired_attempt_integrity_failure", deadline: true, failure: integrityFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
			a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
			if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
				t.Fatalf("initial proof: ready=%v err=%v", ready, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tc.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), time.Millisecond)
			}
			defer cancel()
			if tc.clockDeadline != 0 {
				ctx = pendingTransportDeadlineContext{Context: ctx, deadline: time.Now().Add(tc.clockDeadline)}
				if err := ctx.Err(); err != nil {
					t.Fatalf("pending deadline fixture already canceled: %v", err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("pending deadline fixture already published Done")
				default:
				}
			}
			err := a.closedTransportAuthorizationRefresh(func(attempt context.Context) error {
				if tc.deadline {
					<-attempt.Done() // Native SQL may return ErrBadConn after this timeout.
				}
				if tc.canceled {
					cancel()
				}
				return tc.failure
			})(ctx)
			if !errors.Is(err, tc.failure) {
				t.Fatalf("refresh error=%v, want original %v", err, tc.failure)
			}
			if eligible := eligibility.Eligible(); eligible != tc.keep {
				t.Fatalf("refresh error=%v context=%v eligible=%v, want %v", err, ctx.Err(), eligible, tc.keep)
			}
		})
	}
}

func TestClosedTransportRefreshFailureInvalidatesAndFreshProofRestores(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("initial proof: ready=%v eligible=%v err=%v", ready, eligibility.Eligible(), err)
	}
	quorum := errors.New("quorum unavailable")
	refresh := a.closedTransportAuthorizationRefresh(func(context.Context) error { return quorum })
	if err := refresh(t.Context()); !errors.Is(err, quorum) || eligibility.Eligible() {
		t.Fatalf("failed refresh kept candidate eligible: err=%v eligible=%v", err, eligibility.Eligible())
	}
	if err := a.closedTransportAuthorizationRefresh(func(context.Context) error { return nil })(t.Context()); err != nil {
		t.Fatal(err)
	}
	if eligibility.Eligible() {
		t.Fatal("successful transport refresh alone reopened candidate eligibility")
	}
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("fresh proof did not restore eligibility: ready=%v eligible=%v err=%v", ready, eligibility.Eligible(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.closedTransportAuthorizationRefresh(func(context.Context) error { return nil })(ctx); !errors.Is(err, context.Canceled) || eligibility.Eligible() {
		t.Fatalf("canceled refresh kept candidate eligible: err=%v eligible=%v", err, eligibility.Eligible())
	}
}

func TestClosedMaterialWorkerFailureInvalidatesAndFreshProofRestores(t *testing.T) {
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready {
		t.Fatalf("initial proof: ready=%v err=%v", ready, err)
	}
	a := &App{cfg: Config{DataDir: t.TempDir()}, log: slog.Default(), memberState: memberState{enrollment: &enrollmentServer{materialRequests: newMaterialRequestTracker()}, materialEligibility: eligibility}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { a.runMaterialReplication(ctx, time.Hour); close(done) }()
	deadline := time.After(time.Second)
	for eligibility.Eligible() {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("missing enrollment bundle did not invalidate material eligibility")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if ready, err := eligibility.Reprove(t.Context()); err != nil || !ready || !eligibility.Eligible() {
		t.Fatalf("fresh proof did not restore eligibility: ready=%v eligible=%v err=%v", ready, eligibility.Eligible(), err)
	}
}

func TestClosedMaterialEligibilityRejectsRepeatedRunUntilOldProofDrains(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	eligibility := newMaterialEligibility(time.Now, func(ctx context.Context) (bool, error) {
		close(started)
		<-ctx.Done()
		<-release // deliberately ignore cancellation until the test releases I/O
		return true, nil
	}, time.Hour)
	a := &App{ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, materialEligibility: eligibility}}
	if err := a.startClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-started
	stopCtx, stopCancel := context.WithCancel(t.Context())
	stopCancel()
	if err := a.stopClosedMaterialEligibility(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded Run drain = %v, want canceled", err)
	}
	if err := a.startClosedMaterialEligibility(t.Context()); err == nil {
		t.Fatal("repeated Run overlapped a still-draining proof")
	}
	releaseOnce.Do(func() { close(release) })
	if err := a.stopClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	if eligibility.Eligible() {
		t.Fatal("old proof reopened eligibility after draining")
	}
	fresh := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Hour)
	a.materialEligibilityMu.Lock()
	a.materialEligibility = fresh
	a.materialEligibilityMu.Unlock()
	if err := a.startClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatalf("repeated Run remained blocked after drain: %v", err)
	}
	if err := a.stopClosedMaterialEligibility(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// countedFencedWrites returns a business write behind the store's business
// write barrier, and a reader of which call was the first one admitted: the
// write records its call number on a model whose weight is still unknown.
func countedFencedWrites(t *testing.T, st *store.Store) (write func(string) error, firstAdmitted func() int64) {
	t.Helper()
	model := store.Model{Name: "fenced-write"}
	if err := st.DB().Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	var call int64
	write = func(string) error {
		call++
		return st.FillModelWeight(model.ID, call)
	}
	firstAdmitted = func() int64 {
		t.Helper()
		var got store.Model
		if err := st.DB().First(&got, model.ID).Error; err != nil {
			t.Fatal(err)
		}
		return got.WeightBytes
	}
	return write, firstAdmitted
}
