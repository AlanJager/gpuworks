//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStatusRequiresCurrentBusinessAdmissionAfterQuorumObservation(t *testing.T) {
	database, stop := openStatusPort(t)
	defer stop()
	root := writeStatusRaft(t, statusIdentity(7, database, 0), statusMembers(statusIdentity(7, database, 0)), map[string]string{"metadata1": "m", "0000000000000001-0000000000000001": "s"})
	now := time.Now()
	eligibility := newMaterialEligibility(func() time.Time { return now }, func(context.Context) (bool, error) { return true, nil }, time.Second)
	grantFails := false
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
		return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
			if grantFails {
				return 0, nil, store.ErrClusterAdmissionRejected
			}
			return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
		})
	})
	server := httptest.NewServer(gate)
	defer server.Close()
	previous := statusQuorumFetch
	t.Cleanup(func() { statusQuorumFetch = previous })
	for _, mode := range []string{"closed", "admitted", "grant rejected", "expired during quorum", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			grantFails = mode == "grant rejected"
			if ready, err := eligibility.Reprove(context.Background()); !ready || err != nil {
				t.Fatalf("prepare proof: %t %v", ready, err)
			}
			gate.setBusinessHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/readyz" {
					t.Errorf("unexpected readiness path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write([]byte(`{"status":"ready"}`))
			}))
			if mode == "closed" || mode == "withdrawn" {
				gate.setBusinessHandler(nil)
			}
			statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
				if mode == "expired during quorum" {
					now = now.Add(materialEligibilityWindow)
				}
				return enrollment.ClusterQuorumFacts{LeaderID: 7, Term: 2, Index: 9, ConfigurationIndex: 4, Members: []enrollment.ClusterMember{{ID: 7, Address: database, Role: "voter"}, {ID: 8, Address: database, Role: "voter"}, {ID: 9, Address: database, Role: "voter"}}}, true
			}
			report, err := ObserveClusterStatus(context.Background(), Config{DataDir: root}, server.Listener.Addr().String(), time.Second)
			if mode == "admitted" {
				if err != nil || !strings.Contains(report, "Overall: ready (business reads verified)") {
					t.Fatalf("admitted report: %v\n%s", err, report)
				}
			} else if !errors.Is(err, ErrBusinessStatusNotReady) || !strings.Contains(report, "Overall: not ready") || !strings.Contains(report, "Business: member business service is not ready") {
				t.Fatalf("closed report: %v\n%s", err, report)
			}
			if !strings.Contains(report, "Fault tolerant: true") || !strings.Contains(report, "Sync: observed") {
				t.Fatal("business result erased independently observed raft facts")
			}
			response, err := http.Get(server.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatal("process liveness changed with business readiness")
			}
		})
	}
}

func TestStatusReadinessRejectsEligibilityLossDuringActualSettingsRead(t *testing.T) {
	for _, mode := range []string{"proof expires", "business withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("status-read-loss", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			var advance atomic.Int64
			eligibility := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(advance.Load())) }, func(context.Context) (bool, error) { return true, nil }, time.Second)
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("prepare: %t %v", ready, err)
			}
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			var grants atomic.Int32
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
					grants.Add(1)
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			server := httptest.NewServer(gate)
			t.Cleanup(server.Close)
			entered, resume := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			t.Cleanup(unblock)
			if err := st.DB().Callback().Query().Before("gorm:query").Register("test:pause-readiness-settings", func(tx *gorm.DB) {
				if tx.Statement.Table != "settings" {
					return
				}
				close(entered)
				select {
				case <-resume:
				case <-tx.Statement.Context.Done():
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.DB().Callback().Query().Remove("test:pause-readiness-settings") })
			result := make(chan string, 1)
			go func() { result <- probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("readiness did not reach real settings query")
			}
			if mode == "proof expires" {
				advance.Store(int64(materialEligibilityWindow))
			} else {
				gate.setBusinessHandler(nil)
			}
			unblock()
			select {
			case got := <-result:
				if got == "open" {
					t.Fatal("readiness published success after eligibility loss during actual SQL")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("readiness did not join original SQL read")
			}
			if grants.Load() != 1 {
				t.Fatalf("readiness repeated fresh grant: %d", grants.Load())
			}
		})
	}
}

func TestStatusReadinessRequiresFinalGrantAfterOneRealRead(t *testing.T) {
	for _, mode := range []string{"grant rejected", "grant canceled", "proof expires during final grant", "withdrawn during final grant"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("status-final-grant", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now()
			var advance atomic.Int64
			eligibility := newMaterialEligibility(func() time.Time { return base.Add(time.Duration(advance.Load())) }, func(context.Context) (bool, error) { return true, nil }, time.Second)
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("prepare: %t %v", ready, err)
			}
			gate := newBusinessHandlerGate()
			gate.requireMaterialEligibility()
			gate.setMaterialEligibility(eligibility)
			var grants, reads atomic.Int32
			if err := st.DB().Callback().Query().Before("gorm:query").Register("test:count-readiness-settings", func(tx *gorm.DB) {
				if tx.Statement.Table == "settings" {
					reads.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.DB().Callback().Query().Remove("test:count-readiness-settings") })
			gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
				return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(ctx context.Context, _ store.ClusterIdentity, _ string) (uint64, []store.ClusterTransportMember, error) {
					if grants.Add(1) == 2 {
						if reads.Load() != 1 {
							t.Errorf("final grant before actual business read, or SQL was replayed: %d", reads.Load())
						}
						switch mode {
						case "grant rejected":
							return 0, nil, store.ErrClusterAdmissionRejected
						case "grant canceled":
							<-ctx.Done()
							return 0, nil, ctx.Err()
						case "proof expires during final grant":
							advance.Store(int64(materialEligibilityWindow))
						case "withdrawn during final grant":
							gate.setBusinessHandler(nil)
						}
					}
					return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
				})
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			server := httptest.NewServer(gate)
			t.Cleanup(server.Close)
			if got := probeBusiness(t.Context(), server.Listener.Addr().String(), 200*time.Millisecond); got == "open" {
				t.Fatal("readiness ignored final admission loss")
			}
			server.Close() // Join the canceled final grant before counting physical work.
			if grants.Load() != 2 || reads.Load() != 1 {
				t.Fatalf("grants=%d reads=%d; require two grants, one SQL read", grants.Load(), reads.Load())
			}
		})
	}
}

func TestStatusReadyRequiresRealBusinessSettingsReads(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("status-business-read", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gate := newBusinessHandlerGate()
	var grants atomic.Int32
	gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
		return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
			grants.Add(1)
			return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
		})
	})
	gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st}))
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	if got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second); got != "open" {
		t.Fatalf("healthy business reads rejected: %s", got)
	}
	if err := st.DB().Exec("DROP TABLE settings").Error; err != nil {
		t.Fatal(err)
	}
	if got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second); got == "open" {
		t.Fatal("open admission with failing business settings SQL was reported ready")
	}
	if grants.Load() != 3 {
		t.Fatalf("healthy read needs two grants, failed SQL only one: %d", grants.Load())
	}
}

func TestReadinessDiscardsBufferedSuccessWhenFinalGrantCancelsParent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("status-final-cancellation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var grants atomic.Int32
	gate := newBusinessHandlerGate()
	gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
		return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
			if grants.Add(1) == 2 {
				cancel()
			}
			return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
		})
	})
	gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st}))
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable || grants.Load() != 2 || ctx.Err() == nil {
		t.Fatalf("canceled buffered readiness: status=%d grants=%d ctx=%v", rec.Code, grants.Load(), ctx.Err())
	}
}

func TestStatusReadinessUsesOriginalGrantAndMaterialGuards(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("status-ready-guards", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eligibility := newMaterialEligibility(time.Now, func(context.Context) (bool, error) { return true, nil }, time.Second)
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	var mode, grants atomic.Int32
	gate.setBusinessRequestGuard(func(next http.Handler) http.Handler {
		return guardBusinessRequests(next, store.ClusterIdentity{}, 7, "self", func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
			grants.Add(1)
			switch mode.Load() {
			case 1:
				return 0, nil, store.ErrClusterAdmissionRejected
			case 2:
				eligibility.Invalidate()
			case 3:
				gate.setBusinessHandler(nil)
			}
			return 1, []store.ClusterTransportMember{{ID: 7, KeyFingerprint: "self"}, {ID: 8}}, nil
		})
	})
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	for index, name := range []string{"ready", "grant rejected", "material invalidated during grant", "handler withdrawn during grant", "closed handler"} {
		t.Run(name, func(t *testing.T) {
			mode.Store(int32(index))
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("prepare: %t %v", ready, err)
			}
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			if index == 4 {
				gate.setBusinessHandler(nil)
			}
			before := grants.Load()
			got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second)
			if (got == "open") != (index == 0) {
				t.Fatalf("readiness=%s", got)
			}
			want := int32(1)
			if index == 0 {
				want = 2
			}
			if index == 4 {
				want = 0
			}
			if calls := grants.Load() - before; calls != want {
				t.Fatalf("fresh grants=%d want=%d", calls, want)
			}
			response, err := http.Get(server.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 || grants.Load() != before+want {
				t.Fatal("public liveness changed or bypass extended to readiness")
			}
		})
	}
}

func TestStatusDoesNotDeclareUnobservedBusinessReady(t *testing.T) {
	s := clusterStatus{business: "not observed", httpState: "listening", database: "listening", sync: "observed", faultTolerant: true}
	if report := s.format(); !strings.Contains(report, "Overall: unverified") || !strings.Contains(report, "--http-addr") {
		t.Fatal(report)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	if got := probeBusiness(context.Background(), server.Listener.Addr().String(), time.Second); got == "open" {
		t.Fatal("an anonymous forbidden response was reported as ready")
	}
}

func TestStatusAnonymousAuthenticationFailureDoesNotProveBusinessReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second); got == "open" {
		t.Fatal("anonymous 401 cannot prove authenticated business SQL is available")
	}
}

func TestStatusRequiresExactBusinessReadinessResponse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		body        string
		contentType string
		cache       string
		ready       bool
	}{
		{"ready", 200, `{"status":"ready"}` + "\n", "application/json", "no-store", true},
		{"unauthorized", 401, `{"status":"ready"}`, "application/json", "no-store", false},
		{"forbidden", 403, `{"status":"ready"}`, "application/json", "no-store", false},
		{"closed", 503, "member business service is not ready", "text/plain", "no-store", false},
		{"SQL failure", 503, "business service is not ready", "text/plain", "no-store", false},
		{"oversized", 200, `{"status":"ready"}` + strings.Repeat(" ", 513), "application/json", "no-store", false},
		{"malformed", 200, `{"status":`, "application/json", "no-store", false},
		{"wrong value", 200, `{"status":"ok"}`, "application/json", "no-store", false},
		{"unexpected data", 200, `{"status":"ready","nodes":0}`, "application/json", "no-store", false},
		{"duplicate key", 200, `{"status":"closed","status":"ready"}`, "application/json", "no-store", false},
		{"trailing document", 200, `{"status":"ready"}{}`, "application/json", "no-store", false},
		{"wrong content type", 200, `{"status":"ready"}`, "text/html", "no-store", false},
		{"cacheable", 200, `{"status":"ready"}`, "application/json", "public", false},
		{"redirect", 302, `{"status":"ready"}`, "application/json", "no-store", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/readyz" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Errorf("unexpected readiness request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Cache-Control", tc.cache)
				w.Header().Set("Location", "/other")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			if got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second); (got == "open") != tc.ready {
				t.Fatalf("readiness=%s want ready=%t", got, tc.ready)
			}
		})
	}
}

func TestStatusReadinessRejectsTransportAndIncompleteBody(t *testing.T) {
	if got := probeBusiness(t.Context(), closedStatusPort(t), time.Second); got == "open" {
		t.Fatal("closed listener reported ready")
	}
	for _, mode := range []string{"timeout", "short body"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "timeout" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Length", "100")
				_, _ = w.Write([]byte(`{"status":"ready"}`))
			}))
			defer server.Close()
			if got := probeBusiness(t.Context(), server.Listener.Addr().String(), 50*time.Millisecond); got == "open" {
				t.Fatal("incomplete readiness observation reported ready")
			}
		})
	}
}
