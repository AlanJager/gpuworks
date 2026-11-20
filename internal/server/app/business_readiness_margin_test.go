//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Actual caller, Store/API reads and background Run. Only measured proof age and
// callback release are controlled; production2s scheduling interval is retained.
func TestReadinessRenewalMargin(t *testing.T) {
	for _, mode := range []string{"slow-then-fast", "latest-short-margin", "original-budget", "cancel-request", "epoch-change", "withdraw", "failed-first-then-fast", "final-grant-failure", "final-grant-over-cost"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "margin.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("private-margin", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			const renewalStart = store.ClusterMembershipGrantTimeout - time.Second
			const thinRemaining = store.ClusterMembershipGrantTimeout + 60*time.Millisecond
			const followingRemaining = store.ClusterMembershipGrantTimeout + materialReproofInterval - 260*time.Millisecond
			var age atomic.Int64
			var proofs, reads, grants atomic.Int32
			var followingCompleted atomic.Bool
			firstStarted, nextStarted := make(chan context.Context, 1), make(chan context.Context, 1)
			firstRelease, nextRelease, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var firstOnce, nextOnce sync.Once
			releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
			releaseNext := func() { nextOnce.Do(func() { close(nextRelease) }) }
			clock := func() time.Time {
				return base.Add(time.Duration(age.Load()))
			}
			e := newMaterialEligibility(clock, func(ctx context.Context) (bool, error) {
				switch proofs.Add(1) {
				case 1:
					return true, nil
				case 2:
					firstStarted <- ctx
					select {
					case <-firstRelease:
						age.Store(int64(renewalStart + materialEligibilityWindow - thinRemaining))
						return mode != "failed-first-then-fast", nil
					case <-ctx.Done():
						return false, ctx.Err()
					}
				case 3:
					nextStarted <- ctx
					select {
					case <-nextRelease:
						// First renewal starts one second before the grant timeout, leaving5.06s.
						// A following real Run result must supply the
						// seven-second pregrant reserve, never fabricated eligibility.
						if mode == "latest-short-margin" {
							age.Store(int64(renewalStart + 2*materialEligibilityWindow - thinRemaining - followingRemaining))
						} else {
							age.Store(int64(renewalStart + materialEligibilityWindow - thinRemaining + 100*time.Millisecond))
						}
						followingCompleted.Store(true)
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
				t.Fatalf("initial proof %v %v", ok, err)
			}
			age.Store(int64(renewalStart))
			runCtx, cancelRun := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() { defer close(runDone); e.Run(runCtx) }()
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("Run did not start captured proof")
			}
			age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second))
			const hook = "test:private-margin-real-read"
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
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(ctx context.Context, _ store.ClusterIdentity, _ string) (uint64, []store.ClusterTransportMember, error) {
					n := grants.Add(1)
					if err := ctx.Err(); err != nil {
						return 0, nil, err
					}
					if n == 2 {
						if mode == "slow-then-fast" && !followingCompleted.Load() {
							t.Error("final grant ran before following current proof completed")
						}
						if reads.Load() != 1 {
							t.Errorf("final grant SQLreads%d", reads.Load())
						}
						if mode == "final-grant-failure" {
							return 0, nil, errors.New("injected grant denial")
						}
						cost := 2200 * time.Millisecond
						if mode == "final-grant-over-cost" {
							cost = materialEligibilityWindow - store.ClusterMembershipGrantTimeout + 100*time.Millisecond
						}
						age.Add(int64(cost))
					}
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}))
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
				t.Fatalf("no active wait status%d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("no wait boundary")
			}
			// Measure from completed SQL; pending observation consumes this same budget.
			started := time.Now()
			select {
			case <-done:
				t.Fatalf("readiness completed before its blocked renewal: status%d", rec.Code)
			case <-time.After(25 * time.Millisecond):
			}
			if reads.Load() != 1 || grants.Load() != 1 {
				t.Fatalf("boundary reads/grants%d/%d", reads.Load(), grants.Load())
			}
			switch mode {
			case "cancel-request":
				cancelRequest()
			case "epoch-change":
				e.Invalidate()
			case "withdraw":
				gate.setBusinessHandler(nil)
			case "original-budget":
				time.Sleep(1400 * time.Millisecond)
			}
			releaseFirst()
			select {
			case <-nextStarted:
			case <-done:
				if mode == "slow-then-fast" {
					t.Fatalf("premature final grant: status%d want200 SQL%d grants%d proofs%d", rec.Code, reads.Load(), grants.Load(), proofs.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("following background cycle missing")
			}
			if mode != "original-budget" {
				time.Sleep(20 * time.Millisecond)
				releaseNext()
			}
			select {
			case <-done:
			case <-time.After(materialReproofInterval + time.Second):
				t.Fatal("wait reset/exceeded original cutoff")
			}
			want := http.StatusServiceUnavailable
			if mode == "slow-then-fast" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 {
				t.Fatalf("status%d want%d SQL%d grants%d proofs%d body%q", rec.Code, want, reads.Load(), grants.Load(), proofs.Load(), rec.Body.String())
			}
			if grants.Load() > 2 {
				t.Fatalf("grant replay %d", grants.Load())
			}
			if mode == "slow-then-fast" || mode == "final-grant-failure" || mode == "final-grant-over-cost" {
				if grants.Load() != 2 || proofs.Load() != 3 {
					t.Fatalf("chain SQL%d grants%d proofs%d", reads.Load(), grants.Load(), proofs.Load())
				}
			}
			if mode == "slow-then-fast" {
				if rec.Body.String() != "{\"status\":\"ready\"}\n" || !e.EligibleFor(store.ClusterMembershipGrantTimeout) {
					t.Fatal("publication without final five-second headroom")
				}
			}
			if mode == "original-budget" || mode == "latest-short-margin" {
				// One final grant is permitted on the current five-second proof,
				// but its cost here must still prevent publishing readiness.
				if grants.Load() != 2 {
					t.Fatalf("expected one final grant before reserve rejection: %d", grants.Load())
				}
				elapsed := time.Since(started)
				if elapsed < materialReproofInterval-100*time.Millisecond || elapsed > materialReproofInterval+400*time.Millisecond {
					t.Fatalf("cumulative original2s cutoff %v", elapsed)
				}
			}
			if mode == "failed-first-then-fast" {
				gate.mu.Lock()
				active := gate.active
				gate.mu.Unlock()
				if active != nil {
					t.Fatal("failed result reopened old business handler")
				}
			}
		})
	}
}
