package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMonitorMetricsRoute(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	serve := func(deps Deps, method string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/monitor/metrics", nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		Router(deps).ServeHTTP(rec, req)
		return rec
	}
	withMonitor := Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}}
	if rec := serve(withMonitor, http.MethodGet, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	if rec := serve(withMonitor, http.MethodPost, true); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
	rec := serve(withMonitor, http.MethodGet, true)
	want := `{"items":[{"series":"gpuworks_proxy_ttft_ms","name":"ttft_ms","source":"gateway","kind":"histogram","unit":"milliseconds","quantiles":true}],"total":1}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("status %d body %s\nwant %s", rec.Code, rec.Body.String(), want)
	}
	// Without the monitor service the list is empty, never null.
	if rec := serve(Deps{Store: st, AuthSigner: signer}, http.MethodGet, true); rec.Code != http.StatusOK || rec.Body.String() != `{"items":[],"total":0}`+"\n" {
		t.Fatalf("nil service: %d %s", rec.Code, rec.Body.String())
	}
}
