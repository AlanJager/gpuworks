package gpu

import (
	"context"
	"strings"
	"testing"
)

// Some K100_AI/DTK combinations accept every flag but name utilization HCU
// use (%). A successful command must yield samples without the overview path.
func TestHygonMetricsSuccessfulCombinedQueryRecognizesHCUUse(t *testing.T) {
	calls := 0
	p := &HygonProber{
		command: func(_ context.Context, args ...string) (string, error) {
			calls++
			if got := strings.Join(args, " "); got != "--showuse --showmeminfo vram --showtemp --showpower" {
				t.Fatalf("unexpected fallback query %q", got)
			}
			return readHygonTestdata(t, "hy-smi-combined-hcu.txt"), nil
		},
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 3 || calls != 1 {
		t.Fatalf("Metrics() = %+v, %v; queries=%d", metrics, err, calls)
	}
	for index, want := range []float64{0, 73.5} {
		got, present := metrics[index].Values["gpuworks_gpu_utilization"]
		if !present || got != want {
			t.Errorf("HCU %d utilization = %v (present=%v), want observed %v", index, got, present, want)
		}
	}
	if _, present := metrics[2].Values["gpuworks_gpu_utilization"]; present {
		t.Error("N/A utilization was invented as a sample")
	}
	for index, want := range []float64{41, 37, 56} {
		if got := metrics[index].Values["gpuworks_gpu_temperature_celsius"]; got != want {
			t.Errorf("HCU %d temperature = %v, want %v", index, got, want)
		}
	}
	if got := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; got != 59202*1024*1024 {
		t.Errorf("HCU 0 memory used = %v", got)
	}
	if got := metrics[0].Values["gpuworks_gpu_power_watts"]; got != 122 {
		t.Errorf("HCU 0 power = %v", got)
	}
}
