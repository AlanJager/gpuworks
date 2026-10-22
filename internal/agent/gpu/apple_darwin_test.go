//go:build darwin

package gpu

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// TestAppleProberOnThisMac runs the real commands. It is the check that the
// fixtures still match what macOS prints.
func TestAppleProberOnThisMac(t *testing.T) {
	p := &AppleProber{}
	if !p.Available() {
		t.Skip("not an Apple silicon Mac")
	}
	gpus, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("Probe() = %+v", gpus)
	}
	g := gpus[0]
	t.Logf("%+v", g)
	if g.MemoryTotalBytes <= 0 || g.UUID == "" || g.DriverVersion == "" || accelerator.GPUArch(g.Vendor, g.ArchID, "").Family == "" {
		t.Fatalf("incomplete GPU: %+v", g)
	}
	metrics, err := p.Metrics(context.Background())
	if err != nil || len(metrics) != 1 {
		t.Fatalf("Metrics() = %+v, %v", metrics, err)
	}
	t.Logf("%v", metrics[0].Values)
	for _, key := range []string{"gpuworks_gpu_utilization", "gpuworks_gpu_memory_used_bytes", "gpuworks_gpu_memory_total_bytes"} {
		if _, ok := metrics[0].Values[key]; !ok {
			t.Errorf("metric %s missing", key)
		}
	}
	if New().Vendor() != accelerator.VendorApple {
		t.Fatalf("New() picked %q on an Apple silicon Mac", New().Vendor())
	}
}
