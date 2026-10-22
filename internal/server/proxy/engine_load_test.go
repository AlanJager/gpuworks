package proxy

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestEngineLoadPrunesExpiredInstances(t *testing.T) {
	now := time.Unix(1000, 0)
	load := newEngineLoad()
	load.now = func() time.Time { return now }
	load.observe([]proto.MetricSample{{Metric: "gpuworks_infer_requests_waiting", Labels: map[string]string{"instance": "old"}, Value: 1}})
	if _, ok := load.gauge("old"); !ok {
		t.Fatal("fresh engine gauge missing")
	}
	now = now.Add(engineGaugeMaxAge + time.Second)
	load.observe([]proto.MetricSample{{Metric: "gpuworks_infer_requests_waiting", Labels: map[string]string{"instance": "new"}, Value: 1}})
	if _, ok := load.gauge("old"); ok {
		t.Fatal("expired engine gauge was retained")
	}
}
