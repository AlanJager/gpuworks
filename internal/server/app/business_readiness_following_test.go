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

// Real Reprove/Run publish each result. Only clock age and callback duration
// are controlled: no synthetic deadlines, cached authority or HTTP-started proof.
func TestBusinessReadinessFollowingBackgroundRenewal(t *testing.T) {
	// "failed-first-then-fast" is not run: it expects a first renewal that
	// answers not ready to retire the business generation while the earlier
	// proof is still inside its window. A failed refresh keeps the unexpired
	// earlier proof (material_eligibility.go), so the generation stays.
	for _, mode := range []string{"slow-then-fast", "failed-first", "failed-following", "epoch-change", "epoch-ABA", "cancel-request", "cancel-proof", "stop", "missing", "new-pointer", "withdraw", "second-slow", "original-budget", "original-remaining", "client-deadline", "no-callback", "standalone", "expired-old"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "following.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, token := memberAdminSession(t, st, "following-private")
			base := time.Now()
			const interval = 200 * time.Millisecond
			var age atomic.Int64
			captured := make(chan time.Time, 1)
			var captureOnce sync.Once
			var proofs, reads atomic.Int32
			firstStarted, followingStarted := make(chan context.Context, 1), make(chan context.Context, 1)
			firstRelease, followingRelease, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var firstOnce, followingOnce, failedGenerationOnce sync.Once
			releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
			releaseFollowing := func() { followingOnce.Do(func() { close(followingRelease) }) }
			clock := func() time.Time {
				now := base.Add(time.Duration(age.Load()))
				if reads.Load() == 1 && readinessCaptureClockCaller() {
					captureOnce.Do(func() {
						select {
						case captured <- time.Now():
						default:
						}
					})
				}
				return now
			}
			e := newMaterialEligibility(clock, func(ctx context.Context) (bool, error) {
				n := proofs.Add(1)
				if n == 1 {
					return true, nil
				}
				switch n {
				case 2:
					firstStarted <- ctx
					select {
					case <-firstRelease:
						if mode == "original-remaining" {
							age.Store(int64(materialEligibilityWindow - 20*time.Millisecond))
						} else {
							age.Store(int64(materialEligibilityWindow - 320*time.Millisecond))
						} // Started one second before the grant timeout;4.32s remain at publication.
						return mode != "failed-first" && mode != "failed-first-then-fast", nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				case 3:
					followingStarted <- ctx
					if mode == "failed-first-then-fast" || mode == "epoch-ABA" {
						age.Store(int64(materialEligibilityWindow - 220*time.Millisecond))
						return true, nil
					}
					select {
					case <-followingRelease:
						if mode == "second-slow" {
							age.Store(int64(2*materialEligibilityWindow - store.ClusterMembershipGrantTimeout + 380*time.Millisecond))
						} else {
							age.Store(int64(materialEligibilityWindow - 220*time.Millisecond))
						}
						return mode != "failed-following", nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				default:
					<-ctx.Done()
					return false, ctx.Err()
				}
			}, interval)
			if ready, err := e.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("seed proof: %v %v", ready, err)
			}
			age.Store(int64(store.ClusterMembershipGrantTimeout - time.Second))
			runCtx, cancelRun := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				if mode == "standalone" {
					_, _ = e.Reprove(runCtx)
				} else {
					e.Run(runCtx)
				}
			}()
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("background Run did not begin slow proof")
			}
			age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second)) // Existing authority has four seconds left.
			if mode == "original-remaining" {
				age.Store(int64(materialEligibilityWindow - 100*time.Millisecond))
			}
			const hook = "test:following-real-settings-read"
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
			if mode != "no-callback" {
				e.setOnInvalidated(func() {
					st.SuspendBusinessWrites()
					if mode == "failed-first-then-fast" || mode == "epoch-ABA" {
						failedGenerationOnce.Do(func() {
							// Same synchronous retirement as BeginStop. Hold observation
							// until the following real Run result has been published:
							// the request must miss the temporary zero deadline entirely.
							gate.mu.Lock()
							gate.retire(gate.active)
							gate.active = nil
							go func() {
								select {
								case ctx := <-followingStarted:
									<-ctx.Done() // Successful Reprove publishes before cancel.
								case <-runCtx.Done():
								}
								gate.mu.Unlock()
							}()
						})
						return
					}
					// Production closes Store admission and retires the old route
					// synchronously before Reprove can release its proof slot.
					gate.setBusinessHandler(nil)
				})
			}
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}))
			reqCtx, cancelRequest := context.WithCancel(t.Context())
			if mode == "client-deadline" {
				var cancel context.CancelFunc
				reqCtx, cancel = context.WithTimeout(reqCtx, 90*time.Millisecond)
				defer cancel()
			}
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
				releaseFollowing()
				<-done
				<-runDone
			})
			select {
			case <-readDone:
			case <-done:
				if mode == "client-deadline" {
					// Race instrumentation can exhaust the same client budget
					// inside the actual SQL read, before renewal waiting begins.
					if rec.Code != http.StatusServiceUnavailable || !errors.Is(reqCtx.Err(), context.DeadlineExceeded) || reads.Load() > 1 || proofs.Load() != 2 {
						t.Fatalf("early client deadline changed work: status=%d err=%v reads=%d proofs=%d", rec.Code, reqCtx.Err(), reads.Load(), proofs.Load())
					}
					t.Log("original client deadline expired before renewal wait; actual SQL was not replayed")
					return
				}
				t.Fatalf("did not capture active slow proof: %d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("no readiness wait boundary")
			}
			var waitStarted time.Time
			select {
			case waitStarted = <-captured:
			case <-done:
				if mode == "client-deadline" && errors.Is(reqCtx.Err(), context.DeadlineExceeded) && rec.Code == http.StatusServiceUnavailable && reads.Load() == 1 && proofs.Load() == 2 {
					t.Log("original client deadline expired after SQL before renewal capture; no replay")
					return
				}
				t.Fatalf("readiness completed before capturing its blocked renewal: status%d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("readiness did not capture its blocked renewal")
			}
			if reads.Load() != 1 {
				t.Fatalf("boundary reads=%d", reads.Load())
			}
			switch mode {
			case "epoch-change", "epoch-ABA":
				e.Invalidate()
			case "cancel-request":
				cancelRequest()
			case "cancel-proof":
				e.mu.Lock()
				cancel := e.cancel
				e.mu.Unlock()
				cancel()
			case "stop":
				e.Stop()
			case "missing":
				gate.setMaterialEligibility(nil)
			case "new-pointer":
				other := newMaterialEligibility(clock, func(context.Context) (bool, error) { return true, nil }, interval)
				if ready, err := other.Reprove(t.Context()); !ready || err != nil {
					t.Fatalf("replacement proof %v %v", ready, err)
				}
				gate.setMaterialEligibility(other)
			case "withdraw":
				gate.setBusinessHandler(nil)
			case "expired-old":
				age.Store(int64(materialEligibilityWindow + time.Second))
			}
			if mode == "original-budget" {
				time.Sleep(140 * time.Millisecond)
			}
			if mode == "original-remaining" {
				time.Sleep(60 * time.Millisecond)
			}
			if mode != "expired-old" {
				releaseFirst()
			}
			if mode == "slow-then-fast" || mode == "failed-following" || mode == "second-slow" || mode == "original-budget" || mode == "original-remaining" || mode == "client-deadline" || mode == "new-pointer" || mode == "missing" || mode == "withdraw" {
				select {
				case <-followingStarted:
				case <-time.After(time.Second):
					t.Fatal("Run did not begin the following proof")
				}
				if mode != "original-budget" && mode != "original-remaining" && mode != "client-deadline" {
					time.Sleep(20 * time.Millisecond)
					releaseFollowing()
				}
			}
			select {
			case <-done:
			case <-time.After(interval + time.Second):
				t.Fatal("readiness wait exceeded original allowance")
			}
			want := http.StatusServiceUnavailable
			if mode == "slow-then-fast" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 {
				t.Fatalf("status=%d want%d SQLreads=%d proofs=%d body=%q", rec.Code, want, reads.Load(), proofs.Load(), rec.Body.String())
			}
			if mode == "original-budget" || mode == "original-remaining" || mode == "client-deadline" {
				if elapsed := time.Since(waitStarted); elapsed > interval+100*time.Millisecond {
					t.Fatalf("following proof reset cumulative budget: %v", elapsed)
				}
				e.mu.Lock()
				following := e.proofCtx
				e.mu.Unlock()
				if following == nil || following.Err() != nil {
					t.Fatal("request scheduling cutoff canceled background proof")
				}
			}
			if mode == "original-remaining" {
				if elapsed := time.Since(waitStarted); elapsed < 80*time.Millisecond || elapsed > 190*time.Millisecond {
					t.Fatalf("old remaining lifetime did not cap original wait: %v", elapsed)
				}
			}
			if mode == "failed-first-then-fast" || mode == "epoch-ABA" {
				// Epoch cancellation may reject before owner retirement finishes.
				// Its held lock releases only after next genuine publication.
				gate.mu.Lock()
				active := gate.active
				gate.mu.Unlock()
				if !e.Eligible() || proofs.Load() < 3 {
					t.Fatal("ABA fixture did not publish a genuinely healthy subsequent background proof")
				}
				if active != nil {
					t.Fatal("old business generation reopened after failed proof")
				}
			}
			if mode == "slow-then-fast" {
				if proofs.Load() != 3 || rec.Body.String() != "{\"status\":\"ready\"}\n" {
					t.Fatalf("changed chain proofs=%d body=%q", proofs.Load(), rec.Body.String())
				}
				actual := requestMemberEntry(gate, http.MethodGet, "/api/v1/engines", token)
				if actual.Code != http.StatusOK || reads.Load() != 1 {
					t.Fatalf("following business status=%d SQL=%d", actual.Code, reads.Load())
				}
			}
		})
	}
}

// The first materialEligibility method is the direct caller of its clock.
// Matching any ancestor would mistake current()/EligibleFor clock reads for
// the helper's own sample after captured and original have been saved.
func readinessCaptureClockCaller() bool {
	var pcs [24]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	const method = "gpuworks.ai/gpuworks/internal/server/app.(*materialEligibility)."
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, method) {
			return frame.Function == method+"waitForReadinessRenewal"
		}
		if !more {
			return false
		}
	}
}
