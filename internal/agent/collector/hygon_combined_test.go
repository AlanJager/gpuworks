package collector

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Exercise the real hy-smi command path through collector sample generation:
// zero is an observation with an identity and time, while N/A stays absent.
func TestHygonSuccessfulCombinedQueryEmitsObservedZero(t *testing.T) {
	fixture, err := filepath.Abs("../gpu/testdata/hy-smi-combined-hcu.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$*\" != '--showuse --showmeminfo vram --showtemp --showpower' ]; then exit 91; fi\n/bin/cat %q\n", fixture)
	if err := os.WriteFile(filepath.Join(dir, "hy-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	c := New("hygon-node", &gpu.HygonProber{}, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	samples := c.gpuSamples(t.Context(), 123)
	observed := map[string]proto.MetricSample{}
	for _, sample := range samples {
		if sample.Metric == "gpuworks_gpu_utilization" {
			observed[sample.Labels["gpu"]] = sample
		}
	}
	if len(observed) != 2 {
		t.Fatalf("utilization samples = %+v, want zero and nonzero cards only", observed)
	}
	for index, want := range map[string]float64{"0": 0, "1": 73.5} {
		sample, ok := observed[index]
		if !ok || sample.Value != want || sample.TimeMS == 0 || sample.Labels["node"] != "hygon-node" || sample.Labels["vendor"] != "hygon" || sample.Labels["gpu_uuid"] != "hygon-"+index {
			t.Errorf("HCU %s sample = %+v (present=%v), want observed %v with stable labels", index, sample, ok, want)
		}
	}
}
