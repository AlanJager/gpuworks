package app

import (
	"io"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// runningInstances is a hand-written adapter; this pins that it forwards the
// separate metrics port the instance was launched with, which the collector
// scrapes instead of the API port.
func TestRunningInstancesForwardsTheMetricsPort(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := newNodeRuntime(log, &imagePullTestDriver{}, t.TempDir(), "", metricsOnlyProber{}, nil)
	if n.engine == nil {
		t.Fatal("test requires a node with a container engine")
	}
	n.engine.SetDesiredFull(t.Context(), []proto.InstanceDesiredState{
		{UUID: "own-port", DesiredState: "running", APIPort: 8000, MetricsPort: 8001},
		{UUID: "api-port", DesiredState: "running", APIPort: 8002},
	})
	ports := map[string][2]int{}
	for _, in := range n.runningInstances() {
		ports[in.UUID] = [2]int{in.APIPort, in.MetricsPort}
	}
	if ports["own-port"] != [2]int{8000, 8001} || ports["api-port"] != [2]int{8002, 0} || len(ports) != 2 {
		t.Fatalf("ports = %v", ports)
	}
}
