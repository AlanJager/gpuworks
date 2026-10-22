package collector

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// cachedAnswerProber answers Metrics from a query produced at answeredAt.
type cachedAnswerProber struct {
	recordingGPUProber
	answeredAt time.Time
}

func (p *cachedAnswerProber) Metrics(ctx context.Context) ([]gpu.Metric, error) {
	gpu.RecordResultTime(ctx, p.answeredAt)
	return p.recordingGPUProber.Metrics(ctx)
}

// GPU samples built from an earlier answer carry that answer's time, so a
// slow driver's figures land where they were measured; host samples of the
// same tick keep the tick's time.
func TestGPUSamplesCarryTheAnswerTimeOfACachedQuery(t *testing.T) {
	answered := time.UnixMilli(1_700_000_000_000)
	prober := &cachedAnswerProber{
		recordingGPUProber: recordingGPUProber{metrics: []gpu.Metric{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 50}}}},
		answeredAt:         answered,
	}
	var batch proto.MetricBatch
	c := New("node-1", prober, func() []RunningInstance { return nil }, func(b proto.MetricBatch) { batch = b }, slog.Default())
	before := time.Now().UnixMilli()
	c.RunOnce(context.Background())
	gpuSeen, hostSeen := false, false
	for _, s := range batch.Samples {
		if s.Metric == "gpuworks_gpu_utilization" {
			gpuSeen = true
			if s.TimeMS != answered.UnixMilli() {
				t.Fatalf("gpu sample time = %d, want the answer's %d", s.TimeMS, answered.UnixMilli())
			}
		} else if s.Labels["gpu"] == "" {
			hostSeen = true
			if s.TimeMS < before {
				t.Fatalf("host sample %s took the cached answer's time %d", s.Metric, s.TimeMS)
			}
		}
	}
	if !gpuSeen || !hostSeen {
		t.Fatalf("gpu sample seen=%v host sample seen=%v in %+v", gpuSeen, hostSeen, batch.Samples)
	}

	fresh := &recordingGPUProber{metrics: prober.metrics}
	c = New("node-1", fresh, func() []RunningInstance { return nil }, func(b proto.MetricBatch) { batch = b }, slog.Default())
	if samples := c.gpuSamples(context.Background(), 123); len(samples) == 0 || samples[0].TimeMS != 123 {
		t.Fatalf("a prober that records no answer time keeps the tick's: %+v", samples)
	}
}
