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
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	gate.setMaterialEligibility(eligibility)
	server := httptest.NewServer(gate)
	defer server.Close()
	previous := statusQuorumFetch
	t.Cleanup(func() { statusQuorumFetch = previous })
	for _, mode := range []string{"closed", "admitted", "refused", "expired during quorum", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			if ready, err := eligibility.Reprove(context.Background()); !ready || err != nil {
				t.Fatalf("prepare proof: %t %v", ready, err)
			}
			if mode == "refused" {
				// What a refusal from the database does to this member.
				eligibility.Invalidate()
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
		})
	}
}

// afterReadinessRead runs fn each time the readiness probe's settings read
// has returned its rows: after the real SQL and before the answer is published.
func afterReadinessRead(t *testing.T, st *store.Store, fn func()) {
	t.Helper()
	name := "test:after-readiness-settings:" + t.Name()
	if err := st.DB().Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			fn()
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Callback().Query().Remove(name) })
}

func TestStatusReadinessRechecksAdmissionAfterOneRealRead(t *testing.T) {
	for _, mode := range []string{"refused after the read", "request canceled after the read", "proof expires after the read", "withdrawn after the read"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("status-final-check", time.Hour)
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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var reads atomic.Int32
			afterReadinessRead(t, st, func() {
				if reads.Add(1) != 1 {
					return
				}
				switch mode {
				case "refused after the read":
					eligibility.Invalidate()
				case "request canceled after the read":
					cancel()
				case "proof expires after the read":
					advance.Store(int64(materialEligibilityWindow))
				case "withdrawn after the read":
					gate.setBusinessHandler(nil)
				}
			})
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil).WithContext(ctx))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("readiness ignored admission loss after its read: status=%d", rec.Code)
			}
			if reads.Load() != 1 {
				t.Fatalf("reads=%d; require one SQL read, never replayed", reads.Load())
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
}

func TestStatusReadinessUsesMaterialAndGenerationGuards(t *testing.T) {
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
	server := httptest.NewServer(gate)
	t.Cleanup(server.Close)
	for index, name := range []string{"ready", "material invalidated before the probe", "handler withdrawn before the probe"} {
		t.Run(name, func(t *testing.T) {
			if ready, err := eligibility.Reprove(t.Context()); !ready || err != nil {
				t.Fatalf("prepare: %t %v", ready, err)
			}
			gate.setBusinessHandler(memberServiceHandler(api.Router(api.Deps{Store: st, AuthSigner: signer}), &App{store: st, memberState: memberState{materialEligibility: eligibility}}))
			switch index {
			case 1:
				eligibility.Invalidate()
			case 2:
				gate.setBusinessHandler(nil)
			}
			got := probeBusiness(t.Context(), server.Listener.Addr().String(), time.Second)
			if (got == "open") != (index == 0) {
				t.Fatalf("readiness=%s", got)
			}
			response, err := http.Get(server.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal("public liveness changed with readiness")
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
