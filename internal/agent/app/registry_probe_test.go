package app

import (
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
)

// A real node's pulls rank sources by probing the registries; only an injected
// runtime (a test harness) probes none. Detection fills in the driver, so the
// two must be told apart before it does: a node whose detected runtime counted
// as injected never probed and reported every registry unreachable.
func TestOnlyAnInjectedRuntimeSkipsRegistryProbing(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	detected := newNodeRuntime(log, nil, t.TempDir(), "docker", &gpu.FixedProber{VendorName: "nvidia"}, nil)
	if detected.RuntimeDriverName() != "docker" || !detected.puller.ProbesRegistries() {
		t.Fatalf("detected %q: probes registries = %v, want a probing puller", detected.RuntimeDriverName(), detected.puller.ProbesRegistries())
	}
	injected := newNodeRuntime(log, &imagePullTestDriver{}, t.TempDir(), "", &gpu.FixedProber{VendorName: "nvidia"}, nil)
	if injected.puller.ProbesRegistries() {
		t.Fatal("an injected runtime must not reach the network")
	}
}
