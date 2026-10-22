package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAffinityKeyUsesFirstUserMessageAndStopsBeforeLaterMessages(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":"system"},{"role":"user","content":"same prefix"},{"role":"assistant","content":"large later turn"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	key := affinityKey(req)
	if !strings.Contains(key, `"role":"user"`) || strings.Contains(key, "large later turn") {
		t.Fatalf("affinity key=%q, want first user message without later content", key)
	}
}

func TestAffinityHeaderWinsAndIsRemoved(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-GPUWorks-Affinity-Key", " conversation-42 ")
	if got := affinityKey(req); got != "conversation-42" {
		t.Fatalf("affinity key=%q, want trimmed header", got)
	}
	if req.Header.Get("X-GPUWorks-Affinity-Key") != "" {
		t.Fatal("affinity header was forwarded")
	}
}
