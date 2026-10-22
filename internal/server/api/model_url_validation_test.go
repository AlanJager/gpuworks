package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestURLModelRejectsInvalidURLBeforeNetwork(t *testing.T) {
	st := urlModelStore(t, "api_url_input_validation")
	signer, token := roleTestToken(t, st, "admin")
	router := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, AuthSigner: signer})
	var requests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	if _, err := st.CreatePullProxy(store.PullProxySpec{HTTPProxy: proxy.URL, HTTPSProxy: proxy.URL, ControlPlane: true}, ""); err != nil {
		t.Fatal(err)
	}
	post := func(path, field, raw string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{field: raw})
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for _, endpoint := range []struct{ path, field string }{
		{"/api/v1/models/register-url", "source_url"},
		{"/api/v1/models/preview-url", "url"},
	} {
		for _, raw := range []string{"not-a-url", "http://:8080/models", "https://[]:8080/models", "//mirror.example/models", "ftp://mirror.example/models", "file:///srv/models", "https:///models", "http://mirror.example:invalid/models", "http://mirror.example/%zz", "http://user:secret@mirror.example/models", "http://mirror.example/\x00model"} {
			t.Run(endpoint.field+"/"+raw, func(t *testing.T) {
				before := requests.Load()
				rec := post(endpoint.path, endpoint.field, raw)
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusBadRequest || body["code"] != "MODEL_SOURCE_URL_INVALID" {
					t.Fatalf("status=%d body=%s, want 400/MODEL_SOURCE_URL_INVALID", rec.Code, rec.Body.String())
				}
				if message, _ := body["error"].(string); message == "" || strings.Contains(rec.Body.String(), "secret") {
					t.Fatalf("invalid error envelope: %s", rec.Body.String())
				}
				if requests.Load() != before {
					t.Fatal("invalid URL triggered an outbound request")
				}
			})
		}
		for _, raw := range []string{"http://8.8.8.8:8080/models", "https://8.8.8.8/models", "https://[2606:4700:4700::1111]:8443/models"} {
			before := requests.Load()
			rec := post(endpoint.path, endpoint.field, raw)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"code":"HTTP_DIRECTORY_UNREACHABLE"`) {
				t.Fatalf("valid unreachable URL: %d %s", rec.Code, rec.Body.String())
			}
			if requests.Load() == before {
				t.Fatal("valid URL did not reach the configured network boundary")
			}
		}
	}
	if requests.Load() == 0 {
		t.Fatal("valid URL did not reach the configured network boundary")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var models struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil || rec.Code != 200 || len(models.Items) != 0 {
		t.Fatalf("rejected registration created models: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}

func TestURLModelRoutesRegisterThePreviewedHTTPDirectory(t *testing.T) {
	endpoint := urlModelDirectory(t)
	st := urlModelStore(t, "api_url_input_success")
	signer, token := roleTestToken(t, st, "admin")
	router := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, AuthSigner: signer})
	request := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, rec.Code, rec.Body.String(), want)
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	preview := request(http.MethodPost, "/api/v1/models/preview-url", fmt.Sprintf(`{"url":%q}`, "  "+endpoint+"/?C=N;O=D#weights  "), 200)
	if preview["source_url"] != endpoint || preview["source_id"] != "Qwen/Qwen3-0.6B" || preview["files_count"] != float64(2) || preview["arch"] != "Qwen3ForCausalLM" {
		t.Fatalf("preview = %v", preview)
	}
	empty := request(http.MethodGet, "/api/v1/models", "", 200)
	if len(empty["items"].([]any)) != 0 {
		t.Fatalf("preview registered a model: %v", empty)
	}
	model := request(http.MethodPost, "/api/v1/models/register-url", fmt.Sprintf(`{"source_url":%q}`, endpoint+"///"), 201)
	if model["source"] != "url" || model["source_url"] != endpoint || model["source_id"] != "Qwen/Qwen3-0.6B" {
		t.Fatalf("registered model = %v", model)
	}
	saved := request(http.MethodGet, fmt.Sprintf("/api/v1/models/%.0f", model["id"]), "", 200)
	if saved["source_url"] != endpoint || saved["source_id"] != "Qwen/Qwen3-0.6B" {
		t.Fatalf("saved model = %v", saved)
	}
}
