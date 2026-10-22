//go:build cloudgpu && compshare

package l4

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

type hostedRetryTestForward struct{ baseURL string }

func (f *hostedRetryTestForward) BaseURL() string { return f.baseURL }
func (f *hostedRetryTestForward) Close() error    { return nil }

func TestRegisterHostedModelRetriesInBusinessLayer(t *testing.T) {
	var registerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case "/api/v1/models":
			if registerCalls.Add(1) < 2 {
				http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(dto.Model{ID: 42, Name: "test-model"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var forwardCreations atomic.Int32
	api := newHostedAPIClient(func(context.Context) (hostedForward, error) {
		forwardCreations.Add(1)
		return &hostedRetryTestForward{baseURL: server.URL}, nil
	}, "admin", "password")
	defer api.Close()

	var model dto.Model
	err := registerHostedModel(context.Background(), api, map[string]any{
		"source": "modelscope", "source_id": "example/model",
	}, &model)
	if err != nil {
		t.Fatalf("registerHostedModel() error = %v", err)
	}
	if model.ID != 42 {
		t.Fatalf("registered model = %+v, want ID 42", model)
	}
	if got := registerCalls.Load(); got != 2 {
		t.Fatalf("model registration calls = %d, want 2", got)
	}
	if got := forwardCreations.Load(); got != 2 {
		t.Fatalf("forward creations = %d, want 2 after retry", got)
	}
}
