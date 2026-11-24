//go:build linux && dqlite

package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A reason is logged at its first, second, fourth refusal and so on with its
// own count; a served request after refusals leaves one line with the run's
// size, and a served request with no run open leaves nothing.
func TestBusinessRefusalsAreCountedAndLoggedByReason(t *testing.T) {
	var out bytes.Buffer
	b := &businessRefusals{}
	b.log.Store(slog.New(slog.NewTextHandler(&out, nil)))
	b.served()
	if out.Len() != 0 {
		t.Fatalf("a served request with no refusal before it logged:\n%s", out.String())
	}
	for range 5 {
		b.record(refusalNoKnownLeader)
	}
	b.record(refusalNoGeneration)
	if got := b.snapshot(); got[refusalNoKnownLeader] != 5 || got[refusalNoGeneration] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v", got)
	}
	text := out.String()
	for _, want := range []string{
		`reason=no_known_leader refused=1`, `reason=no_known_leader refused=2`, `reason=no_known_leader refused=4`,
		`reason=no_business_generation refused=1`,
	} {
		if strings.Count(text, want) != 1 {
			t.Fatalf("want one line with %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "level=WARN"); got != 4 {
		t.Fatalf("%d warnings for 6 refusals of 2 reasons, want 4:\n%s", got, text)
	}
	out.Reset()
	b.served()
	b.served()
	text = out.String()
	if strings.Count(text, "business served again after refusals") != 1 || !strings.Contains(text, "level=INFO") || !strings.Contains(text, "refused=6") || !strings.Contains(text, "lasted_ms=") {
		t.Fatalf("serving again after 6 refusals logged:\n%s", text)
	}
	// The counts stay; the next run starts from nothing.
	out.Reset()
	b.record(refusalNoKnownLeader)
	b.served()
	if text = out.String(); !strings.Contains(text, "refused=1 lasted_ms=") {
		t.Fatalf("second run logged:\n%s", text)
	}
	if got := b.snapshot()[refusalNoKnownLeader]; got != 6 {
		t.Fatalf("count after a second run = %d, want 6", got)
	}
}

// Every refusal the gate and the member handler give names its reason in the
// header, keeps the body clients match on, and is counted once; a probe gets
// the header and no count; a served request and the liveness probe get
// neither.
func TestGateAndMemberHandlerRefusalsNameTheirReason(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	upgrading := newBusinessHandlerGate()
	upgrading.clusterUpgrade.Store(true)
	cases := []struct {
		name    string
		handler http.Handler
		path    string
		status  int
		reason  string
		counted bool
		body    string
	}{
		{"gate without a generation", newBusinessHandlerGate(), "/api/v1/nodes", 503, refusalNoGeneration, true, "member business service is not ready"},
		{"gate without a generation, readiness probe", newBusinessHandlerGate(), "/api/v1/readyz", 503, refusalNoGeneration, false, "member business service is not ready"},
		{"gate closed for a cluster upgrade", upgrading, "/api/v1/nodes", 503, refusalClusterUpgrade, true, "CLUSTER_UPGRADE_IN_PROGRESS"},
		{"gate liveness", newBusinessHandlerGate(), "/healthz", 200, "", false, ""},
		{"member handler without business", memberServiceHandler(nil, nil), "/api/v1/nodes", 503, refusalNoHandler, true, "member business service is not ready"},
		{"member handler without a member", memberServiceHandler(ok, nil), "/api/v1/nodes", 503, refusalNoHandler, true, "member business service is not ready"},
		{"member handler liveness", memberServiceHandler(ok, nil), "/healthz", 200, "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := memberBusinessRefusals.snapshot()
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("answered %d %q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(businessRefusalHeader); got != tc.reason {
				t.Fatalf("refusal header %q, want %q", got, tc.reason)
			}
			after := memberBusinessRefusals.snapshot()
			for reason, n := range after {
				want := before[reason]
				if tc.counted && reason == tc.reason {
					want++
				}
				if n != want {
					t.Fatalf("%s counted %d -> %d, want %d", reason, before[reason], n, want)
				}
			}
			if tc.counted && after[tc.reason] == 0 {
				t.Fatalf("%s was not counted", tc.reason)
			}
		})
	}
}

// A request that reaches business closes a run of refusals and is not itself
// counted; a probe that reaches it does not close the run.
func TestServedBusinessEndsARunOfRefusals(t *testing.T) {
	b := &businessRefusals{}
	var out bytes.Buffer
	b.log.Store(slog.New(slog.NewTextHandler(&out, nil)))
	served := b.serving(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	b.record(refusalNoKnownLeader)
	out.Reset()
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if out.Len() != 0 || !b.open.Load() {
		t.Fatalf("a probe ended the run:\n%s", out.String())
	}
	served.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if b.open.Load() || strings.Count(out.String(), "business served again after refusals") != 1 {
		t.Fatalf("a served request did not end the run:\n%s", out.String())
	}
	if got := b.snapshot(); len(got) != 1 || got[refusalNoKnownLeader] != 1 {
		t.Fatalf("serving changed the counts: %v", got)
	}
	if b.serving(nil) != nil {
		t.Fatal("serving(nil) must stay nil: the member handler refuses a missing business handler")
	}
}
