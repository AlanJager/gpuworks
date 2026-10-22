package collector

import (
	"log/slog"
	"strings"
	"testing"
)

// nixlExposition is what prometheus_client prints for vLLM's NIXL connector
// metrics (vllm/distributed/kv_transfer/kv_connector/v1/nixl/stats.py; the
// names were confirmed on vllm/vllm-openai:v0.29.0):
// histograms as _bucket/_sum/_count, counters with the _total suffix and a
// _created companion.
const nixlExposition = `# HELP vllm:nixl_xfer_time_seconds Histogram of transfer duration for NIXL KV Cache transfers.
# TYPE vllm:nixl_xfer_time_seconds histogram
vllm:nixl_xfer_time_seconds_bucket{engine="0",le="0.005",model_name="qwen"} 2.0
vllm:nixl_xfer_time_seconds_bucket{engine="0",le="0.01",model_name="qwen"} 7.0
vllm:nixl_xfer_time_seconds_bucket{engine="0",le="+Inf",model_name="qwen"} 8.0
vllm:nixl_xfer_time_seconds_count{engine="0",model_name="qwen"} 8.0
vllm:nixl_xfer_time_seconds_sum{engine="0",model_name="qwen"} 0.07
# HELP vllm:nixl_bytes_transferred Histogram of bytes transferred per NIXL KV Cache transfers.
# TYPE vllm:nixl_bytes_transferred histogram
vllm:nixl_bytes_transferred_bucket{engine="0",le="2048.0",model_name="qwen"} 0.0
vllm:nixl_bytes_transferred_bucket{engine="0",le="+Inf",model_name="qwen"} 8.0
vllm:nixl_bytes_transferred_count{engine="0",model_name="qwen"} 8.0
vllm:nixl_bytes_transferred_sum{engine="0",model_name="qwen"} 2.68435456e+08
# HELP vllm:nixl_num_failed_transfers_total Number of failed NIXL KV Cache transfers.
# TYPE vllm:nixl_num_failed_transfers_total counter
vllm:nixl_num_failed_transfers_total{engine="0",model_name="qwen"} 3.0
vllm:nixl_num_failed_transfers_created{engine="0",model_name="qwen"} 1.7589e+09
# HELP vllm:nixl_num_kv_expired_reqs_total Number of requests that had their KV expire.
# TYPE vllm:nixl_num_kv_expired_reqs_total counter
vllm:nixl_num_kv_expired_reqs_total{engine="0",model_name="qwen"} 1.0
`

// The vLLM profile's NIXL entries (internal/server/deploy/vllm.go).
var nixlMetricMap = map[string]string{
	"vllm:nixl_xfer_time_seconds":          "gpuworks_infer_nixl_transfer_seconds",
	"vllm:nixl_bytes_transferred":          "gpuworks_infer_nixl_transfer_bytes",
	"vllm:nixl_num_failed_transfers_total": "gpuworks_infer_nixl_failed_transfers_total",
	"vllm:nixl_num_kv_expired_reqs_total":  "gpuworks_infer_nixl_kv_expired_total",
}

func TestNIXLMetricsMapToGpuworksNames(t *testing.T) {
	samples := parsePrometheus(strings.NewReader(nixlExposition), RunningInstance{UUID: "i-pd", Engine: "vllm", MetricMap: nixlMetricMap}, "node-1", 123, slog.Default(), nil)
	got := map[string]float64{}
	for _, s := range samples {
		if s.Labels["instance"] != "i-pd" || s.Labels["node"] != "node-1" {
			t.Fatalf("labels = %+v", s)
		}
		key := s.Metric
		if le := s.Labels["le"]; le != "" {
			key += "{le=" + le + "}"
		}
		got[key] = s.Value
	}
	want := map[string]float64{
		"gpuworks_infer_nixl_transfer_bytes_sum":     268435456,
		"gpuworks_infer_nixl_transfer_bytes_count":   8,
		"gpuworks_infer_nixl_transfer_seconds_sum":   0.07,
		"gpuworks_infer_nixl_transfer_seconds_count": 8,
		"gpuworks_infer_nixl_failed_transfers_total": 3,
		"gpuworks_infer_nixl_kv_expired_total":       1,
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %v, want %v\nall: %v", key, got[key], value, got)
		}
	}
	if _, ok := got["gpuworks_infer_nixl_transfer_seconds_bucket{le=0.01}"]; !ok {
		t.Fatalf("histogram buckets are shipped for quantile folding: %v", got)
	}
	for key := range got {
		if strings.Contains(key, "created") {
			t.Fatalf("a _created companion leaked through: %s", key)
		}
	}
}
