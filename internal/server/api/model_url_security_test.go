package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestURLModelRefusesInternalNetworkBeforeFetching(t *testing.T) {
	st := urlModelStore(t, "api_url_ssrf")
	signer, token := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer internal.Close()
	for _, endpoint := range []struct{ path, field string }{
		{"/api/v1/models/register-url", "source_url"}, {"/api/v1/models/preview-url", "url"},
	} {
		for _, target := range []string{internal.URL, "http://127.0.0.1:9/model", "http://[::1]:9/model", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/model", "http://100.100.100.200/latest/meta-data/"} {
			body, _ := json.Marshal(map[string]string{endpoint.field: target})
			req := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"code":"HTTP_DIRECTORY_UNREACHABLE"`) {
				t.Fatalf("%s target=%s status=%d body=%s", endpoint.path, target, rec.Code, rec.Body.String())
			}
			for _, leak := range []string{"dial tcp", "connection refused", "no such host", "169.254.169.254:80", "SQL"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("network detail leaked: %s", rec.Body.String())
				}
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("internal endpoint received %d requests", hits.Load())
	}
}
