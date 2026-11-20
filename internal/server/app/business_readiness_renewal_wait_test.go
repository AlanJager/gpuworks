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
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBusinessReadinessWaitsForCapturedRenewalBeforeFinalGrant(t *testing.T) {
	for _, mode := range []string{"success", "failed", "invalidate", "stop", "cancel", "proof-cancel", "withdraw", "expired", "missing", "request-deadline", "wait-budget"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "renewal.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, token := memberAdminSession(t, st, "readiness-renewal")
			base := time.Now()
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
					completed.Store(true)
					return mode != "failed", nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}, materialReproofInterval)
			if ready, err := eligibility.Reprove(proofCtx); !ready || err != nil {
				t.Fatalf("initial proof: ready=%v err=%v", ready, err)
			}
			age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second)) // Current proof has four seconds left.
			proofDone := make(chan struct{})
			go func() { defer close(proofDone); _, _ = eligibility.Reprove(proofCtx) }()
			<-entered
			t.Cleanup(func() { stopProof(); eligibility.Stop(); <-proofDone })
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			const hook = "test:readiness-renewal-query"
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
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
					if grants.Add(1) == 2 && mode == "success" && !completed.Load() {
						t.Error("final fresh grant ran before captured renewal finished")
					}
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			if mode == "request-deadline" {
				var deadlineCancel context.CancelFunc
				requestCtx, deadlineCancel = context.WithTimeout(requestCtx, 100*time.Millisecond)
				defer deadlineCancel()
			}
			rec := httptest.NewRecorder()
			requestDone := make(chan struct{})
			go func() {
				defer close(requestDone)
				gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(requestCtx))
			}()
			t.Cleanup(func() { cancelRequest(); <-requestDone })
			select {
			case <-readDone:
			case <-requestDone:
				t.Fatalf("readiness returned before waiting for renewal: status=%d grants=%d", rec.Code, grants.Load())
			case <-time.After(time.Second):
				t.Fatal("readiness did not reach its renewal boundary")
			}
			// Measure from completed SQL; pending observation consumes this same budget.
			waitStarted := time.Now()
			select {
			case <-requestDone:
				if mode != "request-deadline" || requestCtx.Err() != context.DeadlineExceeded || rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("readiness completed before its blocked renewal: status%d", rec.Code)
				}
			case <-time.After(25 * time.Millisecond):
			}
			if grants.Load() != 1 || reads.Load() != 1 {
				t.Fatalf("renewal boundary: grants=%d reads=%d want1/1", grants.Load(), reads.Load())
			}
			switch mode {
			case "invalidate":
				eligibility.Invalidate()
			case "stop":
				eligibility.Stop()
			case "cancel":
				cancelRequest()
			case "proof-cancel":
				stopProof()
			case "withdraw":
				gate.setBusinessHandler(nil)
			case "expired":
				age.Store(int64(2*materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Second + time.Nanosecond))
			case "missing":
				gate.setMaterialEligibility(nil)
			}
			if mode != "request-deadline" && mode != "wait-budget" {
				close(release)
			}
			select {
			case <-requestDone:
			case <-time.After(materialReproofInterval + time.Second):
				t.Fatal("bounded readiness wait did not end")
			}
			if mode == "wait-budget" && time.Since(waitStarted) < materialReproofInterval-100*time.Millisecond {
				t.Fatal("readiness did not wait the original interval for its captured proof")
			}
			want := http.StatusServiceUnavailable
			if mode == "success" {
				want = http.StatusOK
			}
			if rec.Code != want || reads.Load() != 1 || proofs.Load() != 2 {
				t.Fatalf("readyz status=%d want%d reads=%d proofs=%d body=%s", rec.Code, want, reads.Load(), proofs.Load(), rec.Body.String())
			}
			if mode == "success" {
				if grants.Load() != 2 || rec.Body.String() != "{\"status\":\"ready\"}\n" {
					t.Fatalf("ready publication: grants=%d body=%q", grants.Load(), rec.Body.String())
				}
				business := requestMemberEntry(gate, http.MethodGet, "/api/v1/engines", token)
				if business.Code != http.StatusOK || grants.Load() != 3 || reads.Load() != 1 {
					t.Fatalf("next authenticated business: status=%d grants=%d reads=%d", business.Code, grants.Load(), reads.Load())
				}
			}
		})
	}
}
