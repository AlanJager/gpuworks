package traceexport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func TestExistingExporterHonorsPrivatePermissionChanges(t *testing.T) {
	var hits atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer collector.Close()
	var hosts []string
	exporter := New(Options{ReadPrivateHosts: func() []string { return hosts }, AllowPrivateHosts: []string{"127.0.0.1"}})
	defer exporter.Close()
	for _, allowed := range []bool{false, true, false} {
		hosts = nil
		if allowed {
			hosts = []string{"127.0.0.1"}
		}
		_, err := exporter.Test(context.Background(), Config{Endpoint: collector.URL})
		if allowed {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, outbound.ErrTargetDenied) {
			t.Fatalf("unapproved export=%v", err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("unapproved export reached collector: hits=%d", hits.Load())
	}
}
