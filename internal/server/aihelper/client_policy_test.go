package aihelper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func TestExistingAIClientHonorsPrivatePermissionChanges(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ready"},"finish_reason":"stop"}]}`)
	}))
	defer origin.Close()
	var hosts []string
	client := NewClientWithPrivateHosts(Endpoint{BaseURL: origin.URL, Model: "fixture"}, func() []string { return hosts })
	defer client.http.CloseIdleConnections()
	for _, allowed := range []bool{false, true, false} {
		hosts = nil
		if allowed {
			hosts = []string{"127.0.0.1"}
		}
		message, _, err := client.Chat(context.Background(), nil, nil)
		if allowed {
			if err != nil || message.Content != "ready" {
				t.Fatalf("allowed completion=%+v err=%v", message, err)
			}
		} else if !errors.Is(err, outbound.ErrTargetDenied) {
			t.Fatalf("unapproved AI call=%v", err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("unapproved completion reached the endpoint: hits=%d", hits.Load())
	}
}
