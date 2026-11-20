//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReadinessRejectionDiagnosticsPreserveAdmission(t *testing.T) {
	for _, tc := range []struct{ name, stage, reason string }{
		{"entry-proof", "entry", "proof_missing"},
		{"entry-generation", "entry", "no_business_generation"},
		{"initial-grant", "initial_grant", "denied"},
		{"final-grant", "final_grant", "denied"},
		{"epoch", "final_publish", "epoch_changed"},
		{"headroom", "final_publish", "headroom"},
		{"success", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "diagnostics.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("private-readiness-signer", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			// Use the actual closed App construction seam, not a global logger.
			owner := newWaitingClosedMemberApp(&memberRuntime{member: closedMember{cfg: Config{}, log: log}}, nil)
			gate := owner.businessHTTP
			var age atomic.Int64
			base := time.Now()
			var proofs, reads, grants atomic.Int32
			e := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(age.Load())) }, func(context.Context) (bool, error) { proofs.Add(1); return true, nil }, materialReproofInterval)
			if ready, err := e.Reprove(t.Context()); !ready || err != nil {
				t.Fatal(ready, err)
			}
			gate.setMaterialEligibility(e)
			const hook = "test:readiness-rejection-settings"
			if err := st.DB().Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" {
					reads.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(hook) })
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "private-member-key", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
					n := grants.Add(1)
					if (tc.name == "initial-grant" && n == 1) || (tc.name == "final-grant" && n == 2) {
						return 0, nil, errors.New("private-path private-password private-identity")
					}
					if n == 2 {
						if tc.name == "epoch" {
							e.Invalidate()
						}
						if tc.name == "headroom" {
							age.Store(int64(materialEligibilityWindow - store.ClusterMembershipGrantTimeout + time.Nanosecond))
						}
					}
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "private-member-key"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}))
			if tc.name == "entry-proof" {
				e.Invalidate()
			} else if tc.name == "entry-generation" {
				gate.setBusinessHandler(nil)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/readyz?secret=private-query", nil)
			req.Header.Set("X-Request-ID", "private-untrusted-id")
			req.Header.Set("Authorization", "Bearer private-jwt")
			req.Header.Set("Cookie", "private-cookie")
			req.RemoteAddr = "private-address"
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, req)
			if req.Header.Get("X-Request-ID") != "private-untrusted-id" {
				t.Fatal("diagnostic changed the request ID")
			}
			want, wantGrants, wantReads := http.StatusServiceUnavailable, int32(2), int32(1)
			if tc.name == "initial-grant" {
				wantGrants, wantReads = 1, 0
			}
			if strings.HasPrefix(tc.name, "entry-") {
				wantGrants, wantReads = 0, 0
			}
			if tc.name == "success" {
				want = http.StatusOK
			}
			if rec.Code != want || grants.Load() != wantGrants || reads.Load() != wantReads || proofs.Load() != 1 {
				t.Fatalf("behavior changed: status/grants/reads/proofs=%d/%d/%d/%d", rec.Code, grants.Load(), reads.Load(), proofs.Load())
			}
			if tc.name == "success" {
				if output.Len() != 0 || rec.Body.String() != "{\"status\":\"ready\"}\n" {
					t.Fatalf("success diagnostics/response changed: %q / %q", output.String(), rec.Body.String())
				}
				// Compare the exact wire result with the unchanged unlogged gate.
				reference := newBusinessHandlerGate()
				reference.requireMaterialEligibility()
				reference.setMaterialEligibility(e)
				var referenceGrants int
				reference.setBusinessRequestGuard(func(next http.Handler) http.Handler {
					return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "private-member-key", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
						referenceGrants++
						return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "private-member-key"}, {ID: 8}}, nil
					})
				})
				reference.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: e}}))
				beforeReads := reads.Load()
				original := httptest.NewRecorder()
				reference.ServeHTTP(original, req.Clone(req.Context()))
				if original.Code != rec.Code || original.Body.String() != rec.Body.String() || !reflect.DeepEqual(original.Header(), rec.Header()) || referenceGrants != 2 || reads.Load()-beforeReads != 1 {
					t.Fatal("diagnostic changed the successful response or grant/read chain")
				}
				return
			}
			if rec.Body.String() != "member business service is not ready\n" {
				t.Fatal("rejection response changed")
			}
			wantHeaders := map[string]string{"Cache-Control": "no-store", "Content-Type": "text/plain; charset=utf-8", "X-Content-Type-Options": "nosniff"}
			if len(rec.Header()) != len(wantHeaders) {
				t.Fatalf("diagnostic changed rejection headers: %v", rec.Header())
			}
			for name, value := range wantHeaders {
				if rec.Header().Get(name) != value {
					t.Fatalf("rejection header %s changed", name)
				}
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("missing single readiness rejection diagnostic: %v", err)
			}
			if record["msg"] != "business readiness rejected" || record["level"] != "DEBUG" || record["stage"] != tc.stage || record["reason"] != tc.reason {
				t.Fatalf("diagnostic differs: %v", record)
			}
			if strings.Contains(output.String(), "private-") || record["request_id"] != nil {
				t.Fatal("diagnostic exposed private or untrusted request identity")
			}
			if strings.HasPrefix(tc.name, "entry-") {
				if record["elapsed_ms"] != nil {
					t.Fatal("entry invented a measurement or sampled another clock")
				}
			} else if value, ok := record["elapsed_ms"].(float64); !ok || value < 0 || value > 60000 {
				t.Fatal("unbounded elapsed diagnostic")
			}
			if tc.name == "headroom" {
				if value, ok := record["remaining_ms"].(float64); !ok || value < 0 || value >= 5000 || record["required_ms"] != float64(5000) {
					t.Fatalf("missing exact headroom diagnostic: %v", record)
				}
				if !e.Eligible() {
					t.Fatal("headroom diagnostic changed ordinary eligibility")
				}
			}
		})
	}
}
