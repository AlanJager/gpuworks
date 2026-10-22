package monitor

import (
	"testing"
	"time"
)

func TestComputeWindowInferHistograms(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 50}}
	base := map[string]string{"instance": "inst-a", "deployment": "50"}
	withQuantile := func(q string) map[string]string {
		m := map[string]string{"quantile": q}
		for k, v := range base {
			m[k] = v
		}
		return m
	}

	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_infer_ttft_seconds", withQuantile("0.5"), 1.0, at)
		writeSample(t, store, "gpuworks_infer_ttft_seconds", withQuantile("0.95"), 3.0, at)
		writeSample(t, store, "gpuworks_infer_ttft_seconds", withQuantile("0.99"), 4.0, at)
		writeSample(t, store, "gpuworks_infer_tpot_seconds", withQuantile("0.95"), 0.05, at)
		writeSample(t, store, "gpuworks_infer_tpot_seconds", withQuantile("0.99"), 0.08, at)
		writeSample(t, store, "gpuworks_infer_e2e_seconds", withQuantile("0.95"), 5.0, at)
		writeSample(t, store, "gpuworks_infer_queue_time_seconds", withQuantile("0.95"), 0.5, at)
		writeSample(t, store, "gpuworks_infer_prefill_time_seconds", withQuantile("0.95"), 2.1, at)
		writeSample(t, store, "gpuworks_infer_decode_time_seconds", withQuantile("0.95"), 1.9, at)
		writeSample(t, store, "gpuworks_infer_preemptions_total", base, float64(i), at) // 1/tick = 0.1/s
	}

	w := computeWindowFromStore(t, store, 50, instances, baseTime, 5*time.Minute)

	if !w.HasTTFT || w.TTFTP95Sec != 3.0 || w.TTFTP50Sec != 1.0 || w.TTFTP99Sec != 4.0 {
		t.Errorf("TTFT = %+v, want p50=1 p95=3 p99=4", w)
	}
	if !w.HasTPOT || w.TPOTP95Sec != 0.05 || w.TPOTP99Sec != 0.08 {
		t.Errorf("TPOT = %+v, want p95=0.05 p99=0.08", w)
	}
	if !w.HasE2E || w.E2EP95Sec != 5.0 {
		t.Errorf("E2E = %+v, want p95=5", w)
	}
	if !w.HasQueueTime || w.QueueTimeP95Sec != 0.5 {
		t.Errorf("QueueTime = %+v, want p95=0.5", w)
	}
	if !w.HasPrefillTime || w.PrefillTimeP95Sec != 2.1 {
		t.Errorf("PrefillTime = %+v, want p95=2.1", w)
	}
	if !w.HasDecodeTime || w.DecodeTimeP95Sec != 1.9 {
		t.Errorf("DecodeTime = %+v, want p95=1.9", w)
	}
	if got, want := w.PreemptionsPerSec, 0.1; !w.HasPreemptions || got < want-1e-9 || got > want+1e-9 {
		t.Errorf("PreemptionsPerSec = %v has=%v, want 0.1/s (1 per 10s tick)", got, w.HasPreemptions)
	}
}

func TestComputeWindowInferHistogramsAbsentLeavesHasFlagsFalse(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 51}}
	w := computeWindowFromStore(t, store, 51, instances, baseTime, 5*time.Minute)
	if w.HasTTFT || w.HasTPOT || w.HasE2E || w.HasQueueTime || w.HasPrefillTime || w.HasDecodeTime || w.HasPreemptions {
		t.Fatalf("expected every phase2 histogram Has* flag false with no data, got %+v", w)
	}
}

// TestComputeWindowTTFTP95AcrossInstancesTakesMax proves the contract's
// "per instance, then max across instances" reduction: two replicas with
// different p95s must reduce to the larger one, not an average.
func TestComputeWindowTTFTP95AcrossInstancesTakesMax(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{
		{UUID: "a", DeploymentID: 52},
		{UUID: "b", DeploymentID: 52},
	}
	writeSample(t, store, "gpuworks_infer_ttft_seconds", map[string]string{"instance": "a", "deployment": "52", "quantile": "0.95"}, 1.0, baseTime)
	writeSample(t, store, "gpuworks_infer_ttft_seconds", map[string]string{"instance": "b", "deployment": "52", "quantile": "0.95"}, 3.0, baseTime)

	w := computeWindowFromStore(t, store, 52, instances, baseTime, 5*time.Minute)
	if !w.HasTTFT || w.TTFTP95Sec != 3.0 {
		t.Fatalf("TTFTP95Sec = %v has=%v, want 3.0/true (the worse replica)", w.TTFTP95Sec, w.HasTTFT)
	}
}
