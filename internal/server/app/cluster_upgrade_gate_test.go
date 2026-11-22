//go:build linux && dqlite

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A member closed for a cluster upgrade answers with its own code, on the
// same status as any closed member; liveness and the ordinary closed answer
// are unchanged.
func TestClosedGateNamesTheClusterUpgrade(t *testing.T) {
	gate := newBusinessHandlerGate()
	answer := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if w := answer("/api/v1/readyz"); w.Code != http.StatusServiceUnavailable || w.Body.String() != "member business service is not ready\n" {
		t.Fatalf("closed member: %d %q", w.Code, w.Body.String())
	}
	gate.clusterUpgrade.Store(true)
	for _, path := range []string{"/api/v1/readyz", "/api/v1/nodes", "/v1/chat/completions"} {
		w := answer(path)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q is not JSON: %v", path, w.Body.String(), err)
		}
		if w.Code != http.StatusServiceUnavailable || body["code"] != "CLUSTER_UPGRADE_IN_PROGRESS" || w.Header().Get("Retry-After") != "5" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v headers %v", path, w.Code, body, w.Header())
		}
	}
	if w := answer("/healthz"); w.Code != http.StatusOK {
		t.Fatalf("liveness during the upgrade: %d", w.Code)
	}
	gate.clusterUpgrade.Store(false)
	if w := answer("/api/v1/readyz"); w.Body.String() != "member business service is not ready\n" {
		t.Fatalf("after the upgrade: %q", w.Body.String())
	}
}
