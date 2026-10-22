package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type collectorProber struct{ err error }

func (p collectorProber) Vendor() string                                 { return "nvidia" }
func (p collectorProber) Available() bool                                { return p.err == nil }
func (p collectorProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (p collectorProber) Metrics(context.Context) ([]gpu.Metric, error)  { return nil, p.err }
func (p collectorProber) DriverVersion(context.Context) string           { return "test" }
func (p collectorProber) Topology(context.Context) string                { return "" }
func (p collectorProber) RuntimeInjection([]int) gpu.RuntimeInjection    { return gpu.RuntimeInjection{} }

func TestRunOnceEmitsHostSamplesWhenGPUProbeFails(t *testing.T) {
	var mu sync.Mutex
	var got []proto.MetricSample
	c := New("node-1", collectorProber{err: errors.New("smi failed")}, func() []RunningInstance { return nil }, func(batch proto.MetricBatch) { mu.Lock(); got = batch.Samples; mu.Unlock() }, slog.Default())
	c.RunOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("samples=%+v", got)
	}
	for _, sample := range got {
		if sample.Labels["node"] != "node-1" {
			t.Fatalf("sample labels=%+v", sample.Labels)
		}
	}
}

func TestInferSamplesScrapesConfiguredMetricsEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("# HELP x\ncustom_inflight 7\n"))
	})}
	go srv.Serve(listener)
	defer srv.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	c := New("node-1", nil, func() []RunningInstance {
		return []RunningInstance{{UUID: "inst-1", APIPort: port, MetricPath: "/custom", Engine: "custom", MetricMap: map[string]string{"custom_inflight": "gpuworks_infer_requests_running"}}}
	}, nil, slog.Default())
	samples := c.inferSamples(context.Background(), 123)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_running" || samples[0].Value != 7 || samples[0].Labels["engine"] != "custom" {
		t.Fatalf("samples=%+v", samples)
	}
}

func TestGPUMetricSamplesAddsNodeTimeAndLabels(t *testing.T) {
	samples := gpuMetricSamples([]gpu.Metric{{
		Index:  2,
		UUID:   "ascend-2",
		Vendor: "ascend",
		Values: map[string]float64{
			"gpuworks_gpu_memory_total_bytes": 32768 << 20,
			"gpuworks_gpu_utilization":        77,
		},
	}}, "node-1", 123)
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2: %+v", len(samples), samples)
	}
	for _, s := range samples {
		if s.Labels["node"] != "node-1" || s.Labels["gpu"] != "2" || s.Labels["gpu_uuid"] != "ascend-2" || s.Labels["vendor"] != "ascend" {
			t.Fatalf("labels = %+v", s.Labels)
		}
		if s.TimeMS != 123 {
			t.Fatalf("time = %d", s.TimeMS)
		}
	}
}

func TestParsePrometheusNormalizesDefaultVLLM(t *testing.T) {
	body := strings.NewReader(`
# HELP vllm:num_requests_running Number of running requests.
vllm:num_requests_running{model_name="qwen"} 2
vllm:num_requests_waiting 3
vllm:irrelevant_metric 99
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-1"}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2: %+v", len(samples), samples)
	}
	if samples[0].Metric != "gpuworks_infer_requests_running" {
		t.Fatalf("metric = %q", samples[0].Metric)
	}
	if samples[0].Labels["node"] != "node-1" || samples[0].Labels["instance"] != "i-1" || samples[0].Labels["engine"] != "vllm" {
		t.Fatalf("labels = %+v", samples[0].Labels)
	}
	if samples[0].Value != 2 || samples[0].TimeMS != 123 {
		t.Fatalf("sample = %+v", samples[0])
	}
}

// A sample from an instance that belongs to a deployment carries a
// "deployment" label alongside instance/engine, so the server-side monitor
// can group replicas without a per-sample store lookup.
func TestParsePrometheusAddsDeploymentLabelWhenNonZero(t *testing.T) {
	body := strings.NewReader(`vllm:num_requests_running 2`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-1", DeploymentID: 42}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1: %+v", len(samples), samples)
	}
	if samples[0].Labels["deployment"] != "42" {
		t.Fatalf("labels = %+v, want deployment=42", samples[0].Labels)
	}
}

// A legacy/standalone instance (DeploymentID 0) must keep exactly the label
// set it had before deployment labeling existed - no empty "deployment" key.
func TestParsePrometheusOmitsDeploymentLabelWhenZero(t *testing.T) {
	body := strings.NewReader(`vllm:num_requests_running 2`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-1"}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1: %+v", len(samples), samples)
	}
	if _, ok := samples[0].Labels["deployment"]; ok {
		t.Fatalf("labels = %+v, want no deployment key", samples[0].Labels)
	}
}

func TestParsePrometheusUsesEnginePrefix(t *testing.T) {
	body := strings.NewReader(`
sglang:num_requests_running 4
sglang:gpu_cache_usage_perc 0.75
vllm:num_requests_running 99
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-2", Engine: "sglang", MetricPrefix: "sglang"}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2: %+v", len(samples), samples)
	}
	if samples[0].Metric != "gpuworks_infer_requests_running" || samples[1].Metric != "gpuworks_infer_gpu_cache_usage_percent" {
		t.Fatalf("unexpected metrics: %+v", samples)
	}
	if samples[0].Labels["engine"] != "sglang" {
		t.Fatalf("labels = %+v", samples[0].Labels)
	}
}

func TestParsePrometheusUsesExplicitMap(t *testing.T) {
	body := strings.NewReader(`
custom_inflight 5
custom_waiting 8
`)
	samples := parsePrometheus(body, RunningInstance{
		UUID:   "i-3",
		Engine: "custom",
		MetricMap: map[string]string{
			"custom_inflight": "gpuworks_infer_requests_running",
			"custom_waiting":  "gpuworks_infer_requests_waiting",
		},
	}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2: %+v", len(samples), samples)
	}
	if samples[0].Metric != "gpuworks_infer_requests_running" || samples[1].Metric != "gpuworks_infer_requests_waiting" {
		t.Fatalf("unexpected metrics: %+v", samples)
	}
}

// The maps below mirror the deploy profiles verbatim; agent tests may not
// import the server package, so the fixture pins the wire contract instead.
func TestParsePrometheusMapsVLLMCacheAndSpecCounters(t *testing.T) {
	body := strings.NewReader(`
vllm:prefix_cache_queries_total{model_name="qwen"} 10240
vllm:prefix_cache_hits_total{model_name="qwen"} 6144
vllm:spec_decode_num_draft_tokens_total 3200
vllm:spec_decode_num_accepted_tokens_total 2880
vllm:kv_cache_usage_perc 0.55
vllm:prefix_cache_hits 99
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-4", Engine: "vllm", MetricMap: map[string]string{
		"vllm:prefix_cache_queries_total":            "gpuworks_infer_kv_queries_total",
		"vllm:prefix_cache_hits_total":               "gpuworks_infer_kv_hits_total",
		"vllm:spec_decode_num_draft_tokens_total":    "gpuworks_infer_spec_draft_tokens_total",
		"vllm:spec_decode_num_accepted_tokens_total": "gpuworks_infer_spec_accepted_tokens_total",
		"vllm:kv_cache_usage_perc":                   "gpuworks_infer_gpu_cache_usage_percent",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 5 {
		t.Fatalf("samples = %d, want 5: %+v", len(samples), samples)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
	}
	if byName["gpuworks_infer_gpu_cache_usage_percent"] != 0.55 {
		t.Fatalf("cache usage = %+v", byName)
	}
	if byName["gpuworks_infer_kv_queries_total"] != 10240 || byName["gpuworks_infer_kv_hits_total"] != 6144 {
		t.Fatalf("kv counters = %+v", byName)
	}
	if byName["gpuworks_infer_spec_draft_tokens_total"] != 3200 || byName["gpuworks_infer_spec_accepted_tokens_total"] != 2880 {
		t.Fatalf("spec counters = %+v", byName)
	}
}

func TestParsePrometheusMapsSGLangInstantGauges(t *testing.T) {
	body := strings.NewReader(`
sglang:cache_hit_rate 0.42
sglang:spec_accept_rate 0.81
sglang:spec_accept_length 3.2
sglang:token_usage 0.5
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-5", Engine: "sglang", MetricMap: map[string]string{
		"sglang:token_usage":        "gpuworks_infer_gpu_cache_usage_percent",
		"sglang:cache_hit_rate":     "gpuworks_infer_kv_hit_rate_instant",
		"sglang:spec_accept_rate":   "gpuworks_infer_spec_accept_rate",
		"sglang:spec_accept_length": "gpuworks_infer_spec_accept_length",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 4 {
		t.Fatalf("samples = %d, want 4: %+v", len(samples), samples)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
	}
	if byName["gpuworks_infer_kv_hit_rate_instant"] != 0.42 || byName["gpuworks_infer_spec_accept_rate"] != 0.81 || byName["gpuworks_infer_spec_accept_length"] != 3.2 {
		t.Fatalf("instant gauges = %+v", byName)
	}
}

// One engine metric reported once per tier becomes several gpuworks metrics,
// because the samples that reach the time series carry only node, instance and
// engine. The selector picks the row; a tier nobody mapped produces nothing.
func TestParsePrometheusSelectsMetricRowsByLabel(t *testing.T) {
	body := strings.NewReader(`
vllm:kv_offload_tiering_chunk_queries_total{engine="0",tier="0:primary"} 1000
vllm:kv_offload_tiering_chunk_hits_total{engine="0",tier="0:primary"} 800
vllm:kv_offload_tiering_chunk_queries_total{engine="0",tier="1:fs"} 200
vllm:kv_offload_tiering_chunk_hits_total{engine="0",tier="1:fs"} 60
vllm:kv_offload_tiering_chunk_hits_total{engine="0",tier="2:remote"} 7
vllm:kv_offload_tiering_write_bytes_total{tier="1:fs"} 4096
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-6", Engine: "vllm", MetricMap: map[string]string{
		`vllm:kv_offload_tiering_chunk_queries_total{tier="0:primary"}`: "gpuworks_infer_kv_memory_queries_total",
		`vllm:kv_offload_tiering_chunk_hits_total{tier="0:primary"}`:    "gpuworks_infer_kv_memory_hits_total",
		`vllm:kv_offload_tiering_chunk_queries_total{tier="1:fs"}`:      "gpuworks_infer_kv_disk_queries_total",
		`vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}`:         "gpuworks_infer_kv_disk_hits_total",
		`vllm:kv_offload_tiering_write_bytes_total{tier="1:fs"}`:        "gpuworks_infer_kv_disk_write_bytes_total",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 5 {
		t.Fatalf("samples = %d, want 5 (the unmapped 2:remote tier produces none): %+v", len(samples), samples)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
		if s.Labels["node"] != "node-1" || s.Labels["instance"] != "i-6" || s.Labels["engine"] != "vllm" {
			t.Fatalf("labels = %+v; the engine's own labels must not reach the time series", s.Labels)
		}
		if len(s.Labels) != 3 {
			t.Fatalf("labels = %+v, want exactly node/instance/engine", s.Labels)
		}
	}
	want := map[string]float64{
		"gpuworks_infer_kv_memory_queries_total":   1000,
		"gpuworks_infer_kv_memory_hits_total":      800,
		"gpuworks_infer_kv_disk_queries_total":     200,
		"gpuworks_infer_kv_disk_hits_total":        60,
		"gpuworks_infer_kv_disk_write_bytes_total": 4096,
	}
	for metric, value := range want {
		if byName[metric] != value {
			t.Fatalf("%s = %v, want %v: %+v", metric, byName[metric], value, byName)
		}
	}
}

// A bare mapping keeps behaving exactly as it did before selectors existed: it
// matches whether or not the line carries labels, and a selector never steals a
// line whose labels do not satisfy it.
func TestParsePrometheusBareMappingsAreUnaffectedBySelectors(t *testing.T) {
	body := strings.NewReader(`
vllm:num_requests_running{model_name="qwen"} 2
vllm:num_requests_waiting 3
vllm:kv_offload_tiering_chunk_hits_total 11
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-7", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running":                             "gpuworks_infer_requests_running",
		"vllm:num_requests_waiting":                             "gpuworks_infer_requests_waiting",
		`vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}`: "gpuworks_infer_kv_disk_hits_total",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want only the two bare mappings", samples)
	}
	if samples[0].Metric != "gpuworks_infer_requests_running" || samples[0].Value != 2 {
		t.Fatalf("sample = %+v", samples[0])
	}
	if samples[1].Metric != "gpuworks_infer_requests_waiting" || samples[1].Value != 3 {
		t.Fatalf("sample = %+v", samples[1])
	}
}

// A label value the fast path cannot take apart (an embedded comma, an escaped
// quote) simply fails to match, so the line is dropped rather than attributed
// to the wrong tier.
func TestParsePrometheusSkipsLabelValuesItCannotSplit(t *testing.T) {
	body := strings.NewReader(`
vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs,extra"} 5
vllm:kv_offload_tiering_chunk_hits_total{tier="1:\"fs\""} 6
vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"} 7
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-8", Engine: "vllm", MetricMap: map[string]string{
		`vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}`: "gpuworks_infer_kv_disk_hits_total",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 || samples[0].Value != 7 {
		t.Fatalf("samples = %+v, want only the exactly matching tier", samples)
	}
}

// Real vLLM-style histogram exposition: a mapped family's _bucket/_sum/_count
// lines each become one gpuworks-side series, and "+Inf" survives verbatim in
// the "le" label since the server, not the agent, computes quantiles.
func TestParsePrometheusHistogramExpandsBucketsSumAndCount(t *testing.T) {
	body := strings.NewReader(`
# HELP vllm:time_to_first_token_seconds Histogram of TTFT.
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_bucket{le="0.001"} 0
vllm:time_to_first_token_seconds_bucket{le="0.01"} 3
vllm:time_to_first_token_seconds_bucket{le="0.1"} 9
vllm:time_to_first_token_seconds_bucket{le="+Inf"} 12
vllm:time_to_first_token_seconds_sum 4.5
vllm:time_to_first_token_seconds_count 12
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-9", Engine: "vllm", MetricMap: map[string]string{
		"vllm:time_to_first_token_seconds": "gpuworks_infer_ttft_seconds",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 6 {
		t.Fatalf("samples = %d, want 6 (4 buckets + sum + count): %+v", len(samples), samples)
	}
	var buckets int
	var sawSum, sawCount bool
	for _, s := range samples {
		if s.Labels["node"] != "node-1" || s.Labels["instance"] != "i-9" || s.Labels["engine"] != "vllm" {
			t.Fatalf("labels = %+v", s.Labels)
		}
		switch s.Metric {
		case "gpuworks_infer_ttft_seconds_bucket":
			buckets++
			if _, ok := s.Labels["le"]; !ok {
				t.Fatalf("bucket sample missing le label: %+v", s)
			}
		case "gpuworks_infer_ttft_seconds_sum":
			sawSum = true
			if s.Value != 4.5 {
				t.Fatalf("sum = %v, want 4.5", s.Value)
			}
		case "gpuworks_infer_ttft_seconds_count":
			sawCount = true
			if s.Value != 12 {
				t.Fatalf("count = %v, want 12", s.Value)
			}
		default:
			t.Fatalf("unexpected metric %q", s.Metric)
		}
	}
	if buckets != 4 || !sawSum || !sawCount {
		t.Fatalf("buckets=%d sawSum=%v sawCount=%v: %+v", buckets, sawSum, sawCount, samples)
	}
	// "+Inf" must survive verbatim.
	found := false
	for _, s := range samples {
		if s.Metric == "gpuworks_infer_ttft_seconds_bucket" && s.Labels["le"] == "+Inf" {
			found = true
			if s.Value != 12 {
				t.Fatalf("+Inf bucket value = %v, want 12", s.Value)
			}
		}
	}
	if !found {
		t.Fatal("no +Inf bucket sample found")
	}
}

// A histogram family nobody mapped is ignored line by line, exactly like an
// unmapped gauge or counter, even though its base name never appears bare.
func TestParsePrometheusIgnoresUnmappedHistogramFamily(t *testing.T) {
	body := strings.NewReader(`
vllm:time_to_first_token_seconds_bucket{le="0.1"} 1
vllm:time_to_first_token_seconds_sum 0.05
vllm:time_to_first_token_seconds_count 1
vllm:num_requests_running 2
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-10", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running": "gpuworks_infer_requests_running",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_running" {
		t.Fatalf("samples = %+v, want only the mapped gauge", samples)
	}
}

// A _bucket line with no "le" label is malformed exposition and must not be
// guessed at (no gpuworks series should ever be produced without a bucket
// bound), while _sum/_count from the same family are unaffected.
func TestParsePrometheusDropsMalformedBucketLineMissingLe(t *testing.T) {
	body := strings.NewReader(`
vllm:time_to_first_token_seconds_bucket{model_name="qwen"} 5
vllm:time_to_first_token_seconds_sum 1.0
vllm:time_to_first_token_seconds_count 5
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-11", Engine: "vllm", MetricMap: map[string]string{
		"vllm:time_to_first_token_seconds": "gpuworks_infer_ttft_seconds",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want only sum+count (malformed bucket dropped)", samples)
	}
	for _, s := range samples {
		if s.Metric == "gpuworks_infer_ttft_seconds_bucket" {
			t.Fatalf("malformed bucket line must not produce a sample: %+v", samples)
		}
	}
}

// Multiple mapped families in one scrape are each expanded independently,
// and a SGLang-style family (verified against sgl-project/sglang@main) works
// the same way as vLLM's.
func TestParsePrometheusHistogramHandlesMultipleFamilies(t *testing.T) {
	body := strings.NewReader(`
sglang:time_to_first_token_seconds_bucket{le="0.05"} 2
sglang:time_to_first_token_seconds_bucket{le="+Inf"} 5
sglang:time_to_first_token_seconds_sum 0.6
sglang:time_to_first_token_seconds_count 5
sglang:e2e_request_latency_seconds_bucket{le="+Inf"} 5
sglang:e2e_request_latency_seconds_sum 9.0
sglang:e2e_request_latency_seconds_count 5
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-12", Engine: "sglang", MetricMap: map[string]string{
		"sglang:time_to_first_token_seconds": "gpuworks_infer_ttft_seconds",
		"sglang:e2e_request_latency_seconds": "gpuworks_infer_e2e_seconds",
	}}, "node-1", 123, slog.Default(), nil)
	families := map[string]int{}
	for _, s := range samples {
		switch {
		case strings.HasPrefix(s.Metric, "gpuworks_infer_ttft_seconds"):
			families["ttft"]++
		case strings.HasPrefix(s.Metric, "gpuworks_infer_e2e_seconds"):
			families["e2e"]++
		default:
			t.Fatalf("unexpected metric %q", s.Metric)
		}
	}
	if families["ttft"] != 4 || families["e2e"] != 3 {
		t.Fatalf("families = %+v, want ttft=4 (2 buckets+sum+count) e2e=3 (1 bucket+sum+count)", families)
	}
}

// dcgmCallCounter adapts a plain func to dcgmSampler so tests can count and
// script Sample calls without touching the gpu package's unexported PATH
// lookup/exec seams.
type dcgmCallCounter struct {
	calls int
	fn    func(context.Context) (map[int]gpu.DCGMSample, error)
}

func (d *dcgmCallCounter) Sample(ctx context.Context) (map[int]gpu.DCGMSample, error) {
	d.calls++
	if d.fn != nil {
		return d.fn(ctx)
	}
	return nil, nil
}

// Per-tick command-count guard (plan §10): an enabled DCGM probe is sampled
// exactly once per tick, merged into the NVIDIA GPU sample it shares a GPU
// index with.
func TestGPUSamplesCallsDCGMExactlyOnceWhenEnabledOnNVIDIA(t *testing.T) {
	prober := recordingGPUProber{metrics: []gpu.Metric{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 50}}}}
	c := New("node-1", &prober, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
	dcgm := &dcgmCallCounter{fn: func(context.Context) (map[int]gpu.DCGMSample, error) {
		return map[int]gpu.DCGMSample{0: {SMActive: 0.9, SMOccupancy: 0.8, TensorActive: 0.4}}, nil
	}}
	c.dcgm = dcgm
	samples := c.gpuSamples(context.Background(), 123)
	if dcgm.calls != 1 {
		t.Fatalf("dcgm.Sample calls = %d, want exactly 1", dcgm.calls)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
	}
	if byName["gpuworks_gpu_sm_active"] != 0.9 || byName["gpuworks_gpu_sm_occupancy"] != 0.8 || byName["gpuworks_gpu_tensor_active"] != 0.4 {
		t.Fatalf("merged DCGM values = %+v", byName)
	}
	// Called again on a second tick: still exactly one more call, not batched
	// or skipped.
	c.gpuSamples(context.Background(), 456)
	if dcgm.calls != 2 {
		t.Fatalf("dcgm.Sample calls after second tick = %d, want 2", dcgm.calls)
	}
}

// A non-NVIDIA vendor never calls the DCGM probe even if one is attached
// (defense in depth: NewDCGMProbe is NVIDIA-agnostic by construction, but the
// collector must not rely solely on that).
func TestGPUSamplesSkipsDCGMForNonNVIDIAVendor(t *testing.T) {
	prober := recordingGPUProber{metrics: []gpu.Metric{{Index: 0, UUID: "npu-0", Vendor: "ascend", Values: map[string]float64{"gpuworks_gpu_utilization": 50}}}}
	c := New("node-1", &prober, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
	dcgm := &dcgmCallCounter{}
	dcgm.fn = func(context.Context) (map[int]gpu.DCGMSample, error) { return nil, nil }
	// Override vendor via the prober rather than the concrete type; ascend
	// vendor must skip DCGM even though a probe is attached.
	c.dcgm = dcgm
	c.gpuSamples(context.Background(), 123)
	if dcgm.calls != 0 {
		t.Fatalf("dcgm.Sample calls = %d, want 0 for a non-NVIDIA vendor", dcgm.calls)
	}
}

// The default Collector from New has no DCGM probe attached at all: zero
// commands, zero series, no error, matching an agent with no dcgmi on PATH.
func TestGPUSamplesWithoutDCGMProbeAttachedEmitsNoExtraSeries(t *testing.T) {
	prober := recordingGPUProber{metrics: []gpu.Metric{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 50}}}}
	c := New("node-1", &prober, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
	samples := c.gpuSamples(context.Background(), 123)
	for _, s := range samples {
		if strings.HasPrefix(s.Metric, "gpuworks_gpu_sm_") {
			t.Fatalf("no DCGM probe attached must not produce SM series: %+v", samples)
		}
	}
}

// A failing DCGM command is logged once, not once per tick, and produces no
// series for that tick — the rest of the GPU sample is unaffected.
func TestGPUSamplesLogsDCGMFailureOnceAcrossTicks(t *testing.T) {
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	prober := recordingGPUProber{metrics: []gpu.Metric{{Index: 0, UUID: "gpu-0", Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 50}}}}
	c := New("node-1", &prober, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, log)
	dcgm := &dcgmCallCounter{fn: func(context.Context) (map[int]gpu.DCGMSample, error) {
		return nil, errors.New("dcgmi: permission denied")
	}}
	c.dcgm = dcgm
	for i := 0; i < 3; i++ {
		samples := c.gpuSamples(context.Background(), int64(i))
		for _, s := range samples {
			if strings.HasPrefix(s.Metric, "gpuworks_gpu_sm_") {
				t.Fatalf("a failing DCGM command must not produce SM series: %+v", samples)
			}
		}
	}
	if dcgm.calls != 3 {
		t.Fatalf("dcgm.Sample calls = %d, want 3 (still attempted every tick)", dcgm.calls)
	}
	if got := strings.Count(logBuf.String(), "dcgm probe failed"); got != 1 {
		t.Fatalf("dcgm failure log lines = %d, want exactly 1: %s", got, logBuf.String())
	}
}

// A gauge that keeps exposing more than one label combination on every
// scrape must still only log once for the collector's lifetime, not once per
// 10s tick — the same "log once, not once per tick" contract the DCGM
// failure path has above, but keyed per (instance, metric) instead of being
// global to the collector.
func TestInferSamplesWarnsGaugeDuplicateOnceAcrossScrapes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:kv_cache_usage_perc{model_name=\"a\"} 0.5\nvllm:kv_cache_usage_perc{model_name=\"b\"} 0.9\n"))
	})}
	go srv.Serve(listener)
	defer srv.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	c := New("node-1", collectorProber{}, func() []RunningInstance {
		return []RunningInstance{{UUID: "inst-dup", APIPort: port, Engine: "vllm", MetricMap: map[string]string{
			"vllm:kv_cache_usage_perc": "gpuworks_infer_gpu_cache_usage_percent",
		}}}
	}, func(proto.MetricBatch) {}, log)

	const scrapes = 4
	for i := 0; i < scrapes; i++ {
		samples := c.inferSamples(context.Background(), int64(i))
		if len(samples) != 1 || samples[0].Value != 0.5 {
			t.Fatalf("scrape %d samples = %+v, want a single sample keeping 0.5", i, samples)
		}
	}
	if got := strings.Count(logBuf.String(), "multiple label combinations"); got != 1 {
		t.Fatalf("gauge-duplicate warnings across %d scrapes = %d, want exactly 1: %s", scrapes, got, logBuf.String())
	}
}

// recordingGPUProber is a minimal gpu.Prober whose Vendor() reflects its
// fixture metrics' vendor, so DCGM-gating tests can exercise both NVIDIA and
// non-NVIDIA without a second prober type.
type recordingGPUProber struct {
	metrics []gpu.Metric
	err     error
}

func (p *recordingGPUProber) Vendor() string {
	if len(p.metrics) > 0 {
		return p.metrics[0].Vendor
	}
	return "nvidia"
}
func (p *recordingGPUProber) Available() bool                                { return true }
func (p *recordingGPUProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (p *recordingGPUProber) Metrics(context.Context) ([]gpu.Metric, error) {
	// Return a copy so a test that merges DCGM values into it does not leak
	// mutations into the fixture across calls.
	out := make([]gpu.Metric, len(p.metrics))
	for i, m := range p.metrics {
		values := make(map[string]float64, len(m.Values))
		for k, v := range m.Values {
			values[k] = v
		}
		out[i] = gpu.Metric{Index: m.Index, UUID: m.UUID, Vendor: m.Vendor, Values: values}
	}
	return out, p.err
}
func (p *recordingGPUProber) DriverVersion(context.Context) string { return "test" }
func (p *recordingGPUProber) Topology(context.Context) string      { return "" }
func (p *recordingGPUProber) RuntimeInjection([]int) gpu.RuntimeInjection {
	return gpu.RuntimeInjection{}
}

// SetDCGMProbe must clear c.dcgm to a true nil interface when handed a nil
// *gpu.DCGMProbe (as gpu.NewDCGMProbe returns when disabled/absent), not a
// non-nil interface wrapping a nil pointer - the classic Go typed-nil trap
// that would make "c.dcgm != nil" wrongly true.
func TestSetDCGMProbeNilClearsCollectorState(t *testing.T) {
	c := New("node-1", &recordingGPUProber{metrics: []gpu.Metric{{Index: 0, Vendor: "nvidia", Values: map[string]float64{"gpuworks_gpu_utilization": 1}}}}, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
	var nilProbe *gpu.DCGMProbe
	c.SetDCGMProbe(nilProbe)
	if c.dcgm != nil {
		t.Fatalf("SetDCGMProbe(nil) must leave c.dcgm nil, got %#v", c.dcgm)
	}
	c.SetDCGMProbe(&gpu.DCGMProbe{})
	if c.dcgm == nil {
		t.Fatal("SetDCGMProbe(non-nil) must set c.dcgm")
	}
}

func TestSetModelsRootConfiguresHostDiskSampling(t *testing.T) {
	c := New("node-1", collectorProber{}, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
	if c.modelsRoot != "" {
		t.Fatalf("modelsRoot default = %q, want empty", c.modelsRoot)
	}
	c.SetModelsRoot("/data/models")
	if c.modelsRoot != "/data/models" {
		t.Fatalf("modelsRoot = %q, want /data/models", c.modelsRoot)
	}
}

// A faithful vLLM exposition: model_name on every line (the real behavior),
// and vllm:request_success_total broken out per finished_reason. Before the
// dedup fix, three same-label samples reached tsdb and last-write-wins made
// the counter flap between one row's count and the true total; now every row
// is summed into the single gpuworks series exactly once per scrape.
func TestParsePrometheusSumsCounterSplitAcrossLabelCombosInOneScrape(t *testing.T) {
	body := strings.NewReader(`
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{model_name="qwen",finished_reason="stop"} 100000
vllm:request_success_total{model_name="qwen",finished_reason="length"} 50
vllm:request_success_total{model_name="qwen",finished_reason="abort"} 3
vllm:num_requests_running{model_name="qwen"} 5
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-13", Engine: "vllm", MetricMap: map[string]string{
		"vllm:request_success_total": "gpuworks_infer_requests_success_total",
		"vllm:num_requests_running":  "gpuworks_infer_requests_running",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 2 {
		t.Fatalf("samples = %+v, want exactly 2 (one summed counter, one gauge)", samples)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
	}
	if byName["gpuworks_infer_requests_success_total"] != 100053 {
		t.Fatalf("summed counter = %v, want 100053 (100000+50+3)", byName["gpuworks_infer_requests_success_total"])
	}
	if byName["gpuworks_infer_requests_running"] != 5 {
		t.Fatalf("gauge = %v, want 5", byName["gpuworks_infer_requests_running"])
	}
}

// Histogram _bucket/_sum/_count lines are counters too: several label combos
// (here two model_name rows sharing the same "le") must sum into one bucket
// sample, not overwrite each other.
func TestParsePrometheusSumsHistogramAcrossLabelCombosInOneScrape(t *testing.T) {
	body := strings.NewReader(`
vllm:time_to_first_token_seconds_bucket{model_name="a",le="0.1"} 3
vllm:time_to_first_token_seconds_bucket{model_name="b",le="0.1"} 4
vllm:time_to_first_token_seconds_sum{model_name="a"} 0.2
vllm:time_to_first_token_seconds_sum{model_name="b"} 0.3
vllm:time_to_first_token_seconds_count{model_name="a"} 3
vllm:time_to_first_token_seconds_count{model_name="b"} 4
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-14", Engine: "vllm", MetricMap: map[string]string{
		"vllm:time_to_first_token_seconds": "gpuworks_infer_ttft_seconds",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 3 {
		t.Fatalf("samples = %+v, want exactly 3 (bucket+sum+count, each summed once)", samples)
	}
	byName := map[string]float64{}
	for _, s := range samples {
		byName[s.Metric] = s.Value
	}
	if byName["gpuworks_infer_ttft_seconds_bucket"] != 7 {
		t.Fatalf("bucket = %v, want 7 (3+4)", byName["gpuworks_infer_ttft_seconds_bucket"])
	}
	if byName["gpuworks_infer_ttft_seconds_sum"] != 0.5 {
		t.Fatalf("sum = %v, want 0.5", byName["gpuworks_infer_ttft_seconds_sum"])
	}
	if byName["gpuworks_infer_ttft_seconds_count"] != 7 {
		t.Fatalf("count = %v, want 7", byName["gpuworks_infer_ttft_seconds_count"])
	}
}

// A gauge (no _total suffix) exposed under more than one label combination
// cannot be meaningfully summed; the first value seen wins and parsePrometheus
// itself just reports every occurrence to warnGaugeDup — how often that
// becomes one log line is the Collector's job (see
// TestInferSamplesWarnsGaugeDuplicateOnceAcrossScrapes), not this function's.
func TestParsePrometheusGaugeDuplicateKeepsFirstValueAndCallsWarnCallback(t *testing.T) {
	var calls [][2]string
	body := strings.NewReader(`
vllm:kv_cache_usage_perc{model_name="a"} 0.5
vllm:kv_cache_usage_perc{model_name="b"} 0.9
vllm:kv_cache_usage_perc{model_name="c"} 0.1
`)
	samples := parsePrometheus(body, RunningInstance{UUID: "i-15", Engine: "vllm", MetricMap: map[string]string{
		"vllm:kv_cache_usage_perc": "gpuworks_infer_gpu_cache_usage_percent",
	}}, "node-1", 123, slog.Default(), func(instance, metric string) { calls = append(calls, [2]string{instance, metric}) })
	if len(samples) != 1 || samples[0].Value != 0.5 {
		t.Fatalf("samples = %+v, want a single sample keeping the first value 0.5", samples)
	}
	if len(calls) != 2 {
		t.Fatalf("warnGaugeDup calls = %+v, want 2 (the two duplicate lines after the first)", calls)
	}
	for _, c := range calls {
		if c[0] != "i-15" || c[1] != "gpuworks_infer_gpu_cache_usage_percent" {
			t.Fatalf("warnGaugeDup call = %+v", c)
		}
	}
}

// A gauge with no duplicates at all must never call warnGaugeDup.
func TestParsePrometheusNoGaugeDuplicateNeverCallsWarnCallback(t *testing.T) {
	called := false
	body := strings.NewReader(`vllm:kv_cache_usage_perc{model_name="a"} 0.5` + "\n")
	parsePrometheus(body, RunningInstance{UUID: "i-15b", Engine: "vllm", MetricMap: map[string]string{
		"vllm:kv_cache_usage_perc": "gpuworks_infer_gpu_cache_usage_percent",
	}}, "node-1", 123, slog.Default(), func(string, string) { called = true })
	if called {
		t.Fatal("warnGaugeDup must not be called when there is no duplicate")
	}
}

// A scan error (the underlying reader failing mid-scrape) is logged once,
// not silently dropped and not logged per line.
func TestParsePrometheusLogsScanErrorOnce(t *testing.T) {
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	body := "vllm:num_requests_running 2\n"
	r := io.MultiReader(strings.NewReader(body), errReader{err: errors.New("connection reset by peer")})
	samples := parsePrometheus(r, RunningInstance{UUID: "i-16", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running": "gpuworks_infer_requests_running",
	}}, "node-1", 123, log, nil)
	if len(samples) != 1 || samples[0].Value != 2 {
		t.Fatalf("samples = %+v, want the one line read before the error", samples)
	}
	if got := strings.Count(logBuf.String(), "metrics scrape scan error"); got != 1 {
		t.Fatalf("scan-error warnings = %d, want exactly 1: %s", got, logBuf.String())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// A stream longer than the 1 MiB cap gets cut mid-line by parsePrometheus's
// own LimitReader; the final, possibly-partial line (which can still parse
// as a wrong-but-plausible number, e.g. "...42" instead of "...4200") must be
// dropped rather than reported as a real sample.
func TestParsePrometheusDropsPartialFinalLineWhenCapTruncatesStream(t *testing.T) {
	const targetPrefix = "vllm:num_requests_running "
	keptOfTarget := targetPrefix + "42" // what the cap will actually deliver
	fillerLen := (oneMiB + 1) - len(keptOfTarget)
	if fillerLen < 2 {
		t.Fatal("test construction error: filler too short")
	}
	filler := strings.Repeat("#", fillerLen-1) + "\n" // one big ignored comment line
	if len(filler) != fillerLen {
		t.Fatalf("filler length = %d, want %d", len(filler), fillerLen)
	}
	fullTargetLine := targetPrefix + "4200\n" // the real, complete line
	body := strings.NewReader(filler + fullTargetLine)

	samples := parsePrometheus(body, RunningInstance{UUID: "i-17", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running": "gpuworks_infer_requests_running",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 0 {
		t.Fatalf("samples = %+v, want none: the truncated final line (value 42, really 4200) must be dropped, not reported as 42", samples)
	}
}

// The truncation guard must only ever undo a line that actually reached
// add(): a legitimate, complete, mapped sample read well before the cap must
// survive even though the cap itself lands inside a run of unmapped filler
// lines after it. (Before the fix, lastUndo kept pointing at the earlier
// mapped line's undo across every skipped line in between, so truncating
// inside unrelated filler wrongly erased that earlier sample too — proven by
// this test failing with zero samples instead of one on the old code.)
func TestParsePrometheusTruncationInUnmappedFillerDoesNotDropEarlierMappedSample(t *testing.T) {
	mappedLine := "vllm:num_requests_running 7\n"
	unmappedLine := "vllm:other_unmapped_metric 1\n"
	repeats := (oneMiB / len(unmappedLine)) + 10 // comfortably exceeds the cap by itself
	filler := strings.Repeat(unmappedLine, repeats)
	body := strings.NewReader(mappedLine + filler)

	samples := parsePrometheus(body, RunningInstance{UUID: "i-19", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running": "gpuworks_infer_requests_running",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 || samples[0].Value != 7 {
		t.Fatalf("samples = %+v, want the single earlier mapped sample (value 7) preserved", samples)
	}
}

// A body that fits comfortably under the cap is completely unaffected by the
// truncation guard: this proves the guard does not fire on ordinary scrapes.
func TestParsePrometheusDoesNotDropLastLineWhenUnderCap(t *testing.T) {
	body := strings.NewReader("vllm:num_requests_running 4200\n")
	samples := parsePrometheus(body, RunningInstance{UUID: "i-18", Engine: "vllm", MetricMap: map[string]string{
		"vllm:num_requests_running": "gpuworks_infer_requests_running",
	}}, "node-1", 123, slog.Default(), nil)
	if len(samples) != 1 || samples[0].Value != 4200 {
		t.Fatalf("samples = %+v, want the single complete line intact", samples)
	}
}
