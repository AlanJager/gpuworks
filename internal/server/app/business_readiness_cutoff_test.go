//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Real API SQL and Run/Reprove establish the result. The proof clock models a
// measured slow renewal; deadlines are never installed directly by this test.
func TestReadinessPreFinalCutoff(t *testing.T) {
	// "failed-renewal" is not run: it expects a renewal that answers not ready
	// to close a proof still inside its window. A failed refresh keeps the
	// unexpired earlier proof (material_eligibility.go), so that request is
	// answered from the earlier proof instead.
	for _, mode := range []string{"fast-final", "early-thin-proof", "retire-at-cutoff", "epoch-at-cutoff", "cancel-at-cutoff", "proof-cancel-at-cutoff"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "cutoff.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, token := memberAdminSession(t, st, "private-cutoff")
			base := time.Now()
			const renewalStart = store.ClusterMembershipGrantTimeout - time.Second
			const thinRemaining = store.ClusterMembershipGrantTimeout + 60*time.Millisecond
			const followingRemaining = store.ClusterMembershipGrantTimeout + materialReproofInterval - 260*time.Millisecond
			clock := &materialTestClock{now: base}
			anchor := time.Now()
			setAge := func(d time.Duration) {
				clock.mu.Lock()
				clock.now = base.Add(d)
				anchor = time.Now()
				clock.mu.Unlock()
			}
			// The following proof's measured cost is applied by the clock read
			// that publishes its result (Reprove holds the eligibility lock
			// there). Moving the clock when the proof returns instead leaves a
			// moment in which a waiting request reads the new time against
			// the earlier proof's deadline, which a real clock cannot produce.
			var atPublication *time.Duration
			setAgeAtPublication := func(d time.Duration) {
				clock.mu.Lock()
				atPublication = &d
				clock.mu.Unlock()
			}
			var proofs, reads atomic.Int32
			captured := make(chan time.Time, 1)
			var captureOnce sync.Once
			firstStarted, nextStarted := make(chan struct{}), make(chan struct{})
			release, nextRelease, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once, nextOnce sync.Once
			releaseFirst := func() { once.Do(func() { close(release) }) }
			releaseNext := func() { nextOnce.Do(func() { close(nextRelease) }) }
			e := newMaterialEligibility(func() time.Time {
				clock.mu.Lock()
				if atPublication != nil && materialClockReadBy("Reprove") {
					clock.now, anchor, atPublication = base.Add(*atPublication), time.Now(), nil
				}
				now := clock.now.Add(time.Since(anchor))
				clock.mu.Unlock()
				if reads.Load() == 1 && readinessCaptureClockCaller() {
					captureOnce.Do(func() {
						select {
						case captured <- time.Now():
						default:
						}
					})
				}
				return now
			}, func(ctx context.Context) (bool, error) {
				switch proofs.Add(1) {
				case 1:
					return true, nil
				case 2:
					close(firstStarted)
					select {
					case <-release:
						setAge(renewalStart + materialEligibilityWindow - thinRemaining) // First thin real proof leaves5.06 seconds.
						return mode != "failed-renewal", nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				case 3:
					close(nextStarted)
					select {
					case <-nextRelease:
						setAgeAtPublication(renewalStart + 2*materialEligibilityWindow - thinRemaining - followingRemaining) // Following proof leaves6.74 seconds.
						return true, nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				}
				<-ctx.Done()
				return false, ctx.Err()
			}, materialReproofInterval)
			if ok, err := e.Reprove(t.Context()); !ok || err != nil {
				t.Fatalf("seed proof %v %v", ok, err)
			}
			setAge(renewalStart)
			runCtx, cancelRun := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() { defer close(runDone); e.Run(runCtx) }()
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("no real Run renewal")
			}
			// The seed proof has four seconds left; the renewal already started.
			setAge(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second)
			const hook = "test:private-cutoff-query"
			if err := st.DB().Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" {
					if reads.Add(1) == 1 && tx.Error == nil {
						close(readDone)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(hook) })
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(e)
			e.setOnInvalidated(func() { st.SuspendBusinessWrites(); gate.setBusinessHandler(nil) })
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}, nil))
			reqCtx, cancelRequest := context.WithCancel(t.Context())
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(reqCtx))
			}()
			t.Cleanup(func() {
				cancelRequest()
				cancelRun()
				releaseFirst()
				releaseNext()
				<-done
				<-runDone
			})
			select {
			case <-readDone:
			case <-done:
				t.Fatalf("no actual wait status%d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("no wait boundary")
			}
			var started time.Time
			select {
			case started = <-captured:
			case <-done:
				t.Fatalf("readiness completed before capturing its blocked renewal: status%d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("readiness did not capture its blocked renewal")
			}
			if reads.Load() != 1 {
				t.Fatalf("beforewait reads%d", reads.Load())
			}
			releaseFirst()
			if mode == "failed-renewal" {
				select {
				case <-done:
					if rec.Code != http.StatusServiceUnavailable || reads.Load() != 1 || time.Since(started) > materialReproofInterval {
						t.Fatal("definitive negative did not close the original request with one SQL read inside its budget")
					}
				case <-time.After(time.Second):
					t.Fatal("definitive negative did not finish the original request")
				}
				// The request is already closed. Observe the next real proof after
				// Run's existing closed-result interval, without extending HTTP.
				time.Sleep(materialReproofInterval + 20*time.Millisecond)
			}
			select {
			case <-nextStarted:
			case <-time.After(time.Second):
				t.Fatal("no following proof")
			}
			if mode == "failed-renewal" && (e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout)) {
				t.Fatal("unfinished following proof restored a definitive negative result")
			}
			if mode != "early-thin-proof" {
				timer := time.NewTimer(time.Until(started.Add(1700 * time.Millisecond)))
				<-timer.C
			}
			if mode != "proof-cancel-at-cutoff" {
				releaseNext()
			}
			if mode == "retire-at-cutoff" || mode == "epoch-at-cutoff" || mode == "cancel-at-cutoff" || mode == "proof-cancel-at-cutoff" {
				// Gate-level near-cutoff negatives retain actual lifecycle calls.
				timer := time.NewTimer(time.Until(started.Add(materialReproofInterval - 30*time.Millisecond)))
				<-timer.C
				switch mode {
				case "retire-at-cutoff":
					gate.setBusinessHandler(nil)
				case "epoch-at-cutoff":
					e.Invalidate()
				case "cancel-at-cutoff":
					cancelRequest()
				case "proof-cancel-at-cutoff":
					e.mu.Lock()
					cancel := e.cancel
					e.mu.Unlock()
					cancel()
				}
			}
			select {
			case <-done:
			case <-time.After(materialReproofInterval + time.Second):
				t.Fatal("original cutoff reset")
			}
			want := http.StatusServiceUnavailable
			if mode == "fast-final" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 {
				t.Fatalf("status%d want%d SQL%d proofs%d body%q", rec.Code, want, reads.Load(), proofs.Load(), rec.Body.String())
			}
			if mode == "fast-final" {
				elapsed := time.Since(started)
				if elapsed < materialReproofInterval-100*time.Millisecond || elapsed > materialReproofInterval+400*time.Millisecond {
					t.Fatalf("did not use original cumulative2s: %v", elapsed)
				}
				if proofs.Load() < 3 || proofs.Load() > 4 {
					t.Fatalf("published without the following renewal: proofs%d", proofs.Load())
				}
				if rec.Body.String() != "{\"status\":\"ready\"}\n" || !e.EligibleFor(store.ClusterMembershipGrantTimeout) {
					t.Fatal("missing final5 headroom")
				}
				if got := requestMemberEntry(gate, http.MethodGet, "/api/v1/engines", token); got.Code != http.StatusOK {
					t.Fatalf("following actual business status%d", got.Code)
				}
			}
		})
	}
}

// Inject the invalidating event in the existing current-generation predicate
// exactly when its original monotonic cutoff has elapsed. This exercises the
// terminal branch without racing a separate timer against the gate request.
func TestReadinessPreFinalCutoffTerminalFences(t *testing.T) {
	for _, mode := range []string{"retire", "epoch", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			base := time.Now()
			const renewalStart = store.ClusterMembershipGrantTimeout - time.Second
			clock := &materialTestClock{now: base}
			anchor := time.Now()
			setAge := func(d time.Duration) {
				clock.mu.Lock()
				clock.now = base.Add(d)
				anchor = time.Now()
				clock.mu.Unlock()
			}
			clockNow := func() time.Time {
				clock.mu.Lock()
				defer clock.mu.Unlock()
				return clock.now.Add(time.Since(anchor))
			}
			var proofs atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseFirst := func() { once.Do(func() { close(release) }) }
			e := newMaterialEligibility(clockNow, func(ctx context.Context) (bool, error) {
				switch proofs.Add(1) {
				case 1:
					return true, nil
				case 2:
					close(started)
					select {
					case <-release:
						setAge(renewalStart + materialEligibilityWindow - (store.ClusterMembershipGrantTimeout + materialReproofInterval - 200*time.Millisecond))
						return true, nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				default:
					<-ctx.Done()
					return false, ctx.Err()
				}
			}, materialReproofInterval)
			if ok, err := e.Reprove(t.Context()); !ok || err != nil {
				t.Fatalf("seed %v %v", ok, err)
			}
			setAge(renewalStart)
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(e)
			gate.setBusinessHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			captured := gate.active
			e.setOnInvalidated(func() { gate.setBusinessHandler(nil) })
			runCtx, cancelRun := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() { defer close(runDone); e.Run(runCtx) }()
			<-started
			setAge(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second)
			// The actual wall clock advances from this four-second seed reserve.
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(func() { cancel(); cancelRun(); releaseFirst(); <-runDone })
			epoch := e.readinessEpoch()
			waitStarted := time.Now()
			var triggered atomic.Bool
			current := func() bool {
				if time.Since(waitStarted) >= materialReproofInterval && triggered.CompareAndSwap(false, true) {
					switch mode {
					case "retire":
						gate.setBusinessHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
					case "epoch":
						e.Invalidate()
					case "cancel":
						cancel()
					}
				}
				gate.mu.Lock()
				live := gate.active == captured && !captured.retired
				gate.mu.Unlock()
				return live && ctx.Err() == nil && gate.materialEligibility.Load() == e && e.readinessEpochMatches(epoch) && e.Eligible()
			}
			go func() {
				timer := time.NewTimer(1700 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					releaseFirst()
				case <-ctx.Done():
				}
			}()
			err := e.waitForReadinessRenewal(ctx, store.ClusterMembershipGrantTimeout, store.ClusterMembershipGrantTimeout+materialReproofInterval, epoch, current)
			if !triggered.Load() || err == nil {
				t.Fatalf("terminal fence wasn't observed: triggered%v err%v", triggered.Load(), err)
			}
			if mode == "cancel" && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("original request context not canceled")
			}
			if elapsed := time.Since(waitStarted); elapsed > materialReproofInterval+400*time.Millisecond {
				t.Fatalf("cutoff expanded: %v", elapsed)
			}
		})
	}
}

func TestReadinessPreFinalCutoffCapIsNotAuthority(t *testing.T) {
	for _, maxWait := range []time.Duration{300 * time.Millisecond, 0} {
		t.Run(maxWait.String(), func(t *testing.T) {
			base := time.Now()
			const renewalStart = store.ClusterMembershipGrantTimeout - time.Second
			var age atomic.Int64
			var proofs atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseFirst := func() { once.Do(func() { close(release) }) }
			e := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(age.Load())) }, func(ctx context.Context) (bool, error) {
				switch proofs.Add(1) {
				case 1:
					return true, nil
				case 2:
					close(started)
					select {
					case <-release:
						age.Store(int64(materialEligibilityWindow + 500*time.Millisecond))
						return true, nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				default:
					<-ctx.Done()
					return false, ctx.Err()
				}
			}, materialReproofInterval)
			if ok, err := e.Reprove(t.Context()); !ok || err != nil {
				t.Fatalf("seed %v %v", ok, err)
			}
			age.Store(int64(renewalStart))
			e.setOnInvalidated(func() {}) // No failed result is produced in this parameter-bound test.
			runCtx, cancelRun := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() { defer close(runDone); e.Run(runCtx) }()
			<-started
			age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second))
			t.Cleanup(func() { cancelRun(); releaseFirst(); <-runDone })
			go func() {
				timer := time.NewTimer(20 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					releaseFirst()
				case <-runCtx.Done():
				}
			}()
			epoch := e.readinessEpoch()
			current := func() bool { return e.readinessEpochMatches(epoch) && e.Eligible() }
			waitStarted := time.Now()
			err := e.waitForReadinessRenewal(t.Context(), maxWait, store.ClusterMembershipGrantTimeout+materialReproofInterval, epoch, current)
			if err == nil || !e.Eligible() || e.EligibleFor(store.ClusterMembershipGrantTimeout) {
				t.Fatalf("schedulingcap became authority: cap%v err%v", maxWait, err)
			}
			wantWait := materialReproofInterval
			if maxWait > 0 {
				wantWait = maxWait
			}
			if elapsed := time.Since(waitStarted); elapsed < wantWait-30*time.Millisecond || elapsed > wantWait+400*time.Millisecond {
				t.Fatalf("cap changed: %v want%v", elapsed, wantWait)
			}
		})
	}
}

// materialClockReadBy reports whether the eligibility method that is reading
// the clock is the named one.
func materialClockReadBy(name string) bool {
	var pcs [24]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	const method = "gpuworks.ai/gpuworks/internal/server/app.(*materialEligibility)."
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, method) {
			return frame.Function == method+name
		}
		if !more {
			return false
		}
	}
}
