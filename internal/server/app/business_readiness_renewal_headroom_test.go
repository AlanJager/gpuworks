//go:build linux && dqlite

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBusinessReadinessRenewalReservesFinalGrantHeadroom(t *testing.T) {
	for _, mode := range []string{"renewed-before-final-grant", "renewal-exceeds-wait", "renewed-proof-exceeds-five-seconds"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "headroom.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("renewal-headroom", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			const ageBeforeFinal = materialEligibilityWindow - store.ClusterMembershipGrantTimeout - 218*time.Millisecond //5.218 seconds remain.
			const finalGrantCost = 287 * time.Millisecond
			var age atomic.Int64
			var proofs, reads, grants atomic.Int32
			var completed atomic.Bool
			entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			proofCtx, stopProof := context.WithCancel(t.Context())
			defer stopProof()
			eligibility := newMaterialEligibility(func() time.Time {
				return base.Add(time.Duration(age.Load()))
			}, func(ctx context.Context) (bool, error) {
				if proofs.Add(1) == 1 {
					return true, nil
				}
				close(entered)
				select {
				case <-release:
					elapsed := 100 * time.Millisecond
					if mode == "renewed-proof-exceeds-five-seconds" {
						elapsed = materialEligibilityWindow - store.ClusterMembershipGrantTimeout + 13*time.Millisecond
					}
					// The production component measures proof age with this same
					// controlled clock. No TTL or operation timeout is relaxed.
					age.Store(int64(ageBeforeFinal + elapsed))
					completed.Store(true)
					return true, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}, materialReproofInterval)
			if ready, err := eligibility.Reprove(proofCtx); !ready || err != nil {
				t.Fatalf("initial proof ready=%v err=%v", ready, err)
			}
			age.Store(int64(ageBeforeFinal))
			proofDone := make(chan struct{})
			go func() { defer close(proofDone); _, _ = eligibility.Reprove(proofCtx) }()
			<-entered
			t.Cleanup(func() { stopProof(); eligibility.Stop(); <-proofDone })
			const hook = "test:readiness-renewal-headroom"
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
			gate.setMaterialEligibility(eligibility)
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
					if grants.Add(1) == 2 {
						if mode != "renewal-exceeds-wait" && !completed.Load() {
							t.Error("final fresh grant ran before the captured renewal")
						}
						age.Add(int64(finalGrantCost))
					}
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(requestCtx))
			}()
			t.Cleanup(func() { cancelRequest(); <-done })
			select {
			case <-readDone:
			case <-done:
				t.Fatalf("readyz skipped its near-boundary renewal: status=%d", rec.Code)
			case <-time.After(time.Second):
				t.Fatal("readiness did not reach renewal scheduling")
			}
			// Measure from completed SQL; pending observation consumes this same budget.
			waitStarted := time.Now()
			select {
			case <-done:
				t.Fatalf("readiness completed before its blocked renewal: status%d", rec.Code)
			case <-time.After(25 * time.Millisecond):
			}
			if grants.Load() != 1 || reads.Load() != 1 {
				t.Fatalf("near-boundary renewal bypassed: grants=%d reads=%d want1/1", grants.Load(), reads.Load())
			}
			if mode != "renewal-exceeds-wait" {
				close(release)
			}
			select {
			case <-done:
			case <-time.After(materialReproofInterval + time.Second):
				t.Fatal("readiness exceeded its original bounded renewal wait")
			}
			want := http.StatusServiceUnavailable
			if mode == "renewed-before-final-grant" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 || grants.Load() != 2 || proofs.Load() != 2 {
				t.Fatalf("headroom result: status=%d want%d reads=%d grants=%d proofs=%d", rec.Code, want, reads.Load(), grants.Load(), proofs.Load())
			}
			if mode == "renewal-exceeds-wait" && (completed.Load() || time.Since(waitStarted) < materialReproofInterval-100*time.Millisecond) {
				t.Fatal("slow renewal did not retain the original two-second cutoff")
			}
			if mode == "renewed-proof-exceeds-five-seconds" && (!eligibility.Eligible() || eligibility.EligibleFor(store.ClusterMembershipGrantTimeout)) {
				t.Fatal("slow successful proof changed ordinary eligibility or bypassed final five-second headroom")
			}
		})
	}
}
