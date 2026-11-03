package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRunningEngineClosedPortIsReportedFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/is_sleeping" {
			_, _ = w.Write([]byte("false"))
		}
	}))
	t.Cleanup(server.Close)
	driver := newLivenessDriver()
	e, reports := newLivenessEngine(t, driver)
	spec := runningSpec("closed-engine-port", serverPort(t, server))
	spec.Engine = engines.VLLM
	serve(t, e, reports, spec)
	server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.checkLiveness(ctx)
	failed := reports.waitFor(t, proto.InstFailed, 1)
	if failed[0].UUID != spec.UUID || !strings.Contains(failed[0].Message, "engine health check failed") {
		t.Fatalf("failure does not explain the lost service: %+v", failed)
	}
	if driver.exists(containerName(spec.UUID)) {
		t.Fatal("failed engine's container was not reclaimed")
	}
}
