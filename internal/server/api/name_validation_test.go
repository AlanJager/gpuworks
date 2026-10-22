package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteHandlersRejectUnsafeNames(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	h := &apiHandlers{d: Deps{Store: rig.st}}
	for name, handler := range map[string]http.HandlerFunc{
		"api key":     h.createAPIKey,
		"model":       h.createModel,
		"quick model": h.quickRegisterModel,
		"AI config":   h.createAIConfig,
		"deployment":  h.createDeployment,
		"network":     h.createNetwork,
	} {
		t.Run(name, func(t *testing.T) {
			for _, value := range []string{strings.Repeat("a", 257), "bad\x00name", "bad\nname"} {
				body, _ := json.Marshal(map[string]any{"name": value, "source": "huggingface", "source_id": "org/model", "browser_metadata": true})
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
				rec := httptest.NewRecorder()
				handler(rec, req)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
					t.Fatalf("unsafe name length %d: %d %s", len(value), rec.Code, rec.Body)
				}
			}
		})
	}
}

func TestModelDefaultNamesAreBounded(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	h := &apiHandlers{d: Deps{Store: rig.st}}
	for _, handler := range []http.HandlerFunc{h.createModel, h.quickRegisterModel} {
		body, _ := json.Marshal(map[string]any{"source": "huggingface", "source_id": strings.Repeat("a", 257), "browser_metadata": true})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
			t.Fatalf("default name bypassed validation: %d %s", rec.Code, rec.Body)
		}
	}
}

func TestSmallWritesRejectTrailingAndOversizedJSON(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	h := &apiHandlers{d: Deps{Store: rig.st}}
	for name, handler := range map[string]http.HandlerFunc{"api key": h.createAPIKey, "AI config": h.createAIConfig, "deployment": h.createDeployment} {
		t.Run(name, func(t *testing.T) {
			for _, body := range []string{`{"name":"valid"} {}`, `{"name":"valid","padding":"` + strings.Repeat("a", 2<<20) + `"}`} {
				rec := httptest.NewRecorder()
				handler(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
				if (rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge) || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
					t.Fatalf("invalid body length %d: %d %s", len(body), rec.Code, rec.Body)
				}
			}
		})
	}
}
