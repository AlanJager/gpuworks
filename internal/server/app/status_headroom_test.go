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

func TestStatusReadinessRejectsProofAboutToExpire(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		status    int
		duringSQL bool
	}{
		{"fresh", 0, http.StatusOK, false},
		{"full grant timeout remains", materialEligibilityWindow - store.ClusterMembershipGrantTimeout, http.StatusOK, false},
		{"more than a grant timeout remains", materialEligibilityWindow - store.ClusterMembershipGrantTimeout - time.Nanosecond, http.StatusOK, false},
		{"less than a grant timeout remains", materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Nanosecond, http.StatusServiceUnavailable, false},
		{"real SQL consumes headroom", materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Nanosecond, http.StatusServiceUnavailable, true},
		{"observed ready then login failure window", materialEligibilityWindow - 218*time.Millisecond, http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("status-headroom", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			var age atomic.Int64
			var proofs atomic.Int32
			eligibility := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(age.Load())) }, func(context.Context) (bool, error) { proofs.Add(1); return true, nil }, time.Second)
			var invalidations atomic.Int32
			eligibility.setOnInvalidated(func() { invalidations.Add(1) })
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("prove: %t %v", ready, err)
			}
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			var reads atomic.Int32
			if err := st.DB().Callback().Query().Before("gorm:query").Register("test:headroom-settings", func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" {
					if reads.Add(1) == 1 && tc.duringSQL {
						age.Store(int64(tc.age))
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.DB().Callback().Query().Remove("test:headroom-settings") })
			var published atomic.Int32
			afterReadinessRead(t, st, func() {
				// The time the probe's own work took, after its one real read.
				if published.Add(1) == 1 && !tc.duringSQL {
					age.Store(int64(tc.age))
				}
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}, nil))
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
			if rec.Code != tc.status {
				t.Fatalf("readyz status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if reads.Load() != 1 {
				t.Fatalf("readiness did not make exactly one real read: reads=%d", reads.Load())
			}
			if !eligibility.Eligible() || invalidations.Load() != 0 {
				t.Fatal("readiness headroom changed ordinary business eligibility")
			}
			if tc.status == http.StatusOK && rec.Body.String() != "{\"status\":\"ready\"}\n" {
				t.Fatalf("unexpected ready body: %q", rec.Body.String())
			}
			// A conservative readiness rejection leaves ordinary admission open.
			business := httptest.NewRecorder()
			gate.ServeHTTP(business, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
			if business.Code != http.StatusUnauthorized {
				t.Fatalf("ordinary business was withdrawn: status=%d", business.Code)
			}
			if proofs.Load() != 1 {
				t.Fatal("HTTP readiness started a material scan")
			}
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("refresh: %t %v", ready, err)
			}
			refreshed := httptest.NewRecorder()
			gate.ServeHTTP(refreshed, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
			if refreshed.Code != http.StatusOK || proofs.Load() != 2 || reads.Load() != 2 || invalidations.Load() != 0 {
				t.Fatalf("complete reproof did not restore readiness without side effects: status=%d proofs=%d reads=%d invalidations=%d", refreshed.Code, proofs.Load(), reads.Load(), invalidations.Load())
			}

		})
	}
}
