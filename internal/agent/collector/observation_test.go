package collector

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// tensorfoldRules is the mapping the built-in GLM-5.3-Flash TensorFold
// templates declare, as the server resolves it into an observation.
var tensorfoldRules = []proto.MetricRule{
	{Source: "tensorfold:mtp_accepted_total", Series: "gpuworks_infer_spec_accepted_tokens_total"},
	{Source: "tensorfold:mtp_drafted_total", Series: "gpuworks_infer_spec_draft_tokens_total"},
	{Source: "tensorfold:prompt_tokens_total", Series: "gpuworks_infer_prompt_tokens_total"},
	{Source: "tensorfold:request_latency_seconds", Series: "gpuworks_infer_e2e_seconds"},
	{Source: "tensorfold:requests_running", Series: "gpuworks_infer_requests_running"},
	{Source: "tensorfold:requests_waiting", Series: "gpuworks_infer_requests_waiting"},
	{Source: "tensorfold:time_to_first_token_seconds", Series: "gpuworks_infer_ttft_seconds"},
	{Source: "tensorfold_health:completion_tokens_total", Series: "gpuworks_infer_generation_tokens_total"},
	{Source: "tensorfold_health:cached_tokens_total", Series: "gpuworks_infer_prompt_tokens_cache_hit_total"},
	{Source: "tensorfold_health:requests_total", Series: "gpuworks_infer_requests_success_total"},
	{Source: "tensorfold_health:pool_free_tokens", Series: "gpuworks_infer_kv_free_tokens"},
	{Source: "tensorfold_health:pool_tokens", Series: "gpuworks_infer_kv_capacity_tokens"},
}

func tensorfoldScrape(t *testing.T) string {
	t.Helper()
	// A real /metrics response of TensorFold 0.6.0 on the DGX Spark pair.
	data, err := os.ReadFile("../../inferencemetrics/testdata/tensorfold-gb10.metrics")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sampleIndex(samples []proto.MetricSample) map[string]proto.MetricSample {
	index := make(map[string]proto.MetricSample, len(samples))
	for _, sample := range samples {
		key := sample.Metric
		if le, ok := sample.Labels["le"]; ok {
			key += "{le=" + le + "}"
		}
		index[key] = sample
	}
	return index
}

func TestObservationMapsARealTensorFoldScrapeAndDerivesKVUsage(t *testing.T) {
	in := RunningInstance{UUID: "tf-1", Engine: "callable", DeploymentID: 12, Observation: &proto.InstanceObservation{
		MetricPath: "/metrics", MetricRules: tensorfoldRules,
	}}
	samples := parsePrometheus(strings.NewReader(tensorfoldScrape(t)), in, "node-1", 123, slog.Default(), nil)
	got := sampleIndex(samples)

	want := map[string]float64{
		"gpuworks_infer_requests_running":              0,
		"gpuworks_infer_requests_waiting":              0,
		"gpuworks_infer_prompt_tokens_total":           645482,
		"gpuworks_infer_generation_tokens_total":       264943,
		"gpuworks_infer_spec_draft_tokens_total":       304151,
		"gpuworks_infer_spec_accepted_tokens_total":    160360,
		"gpuworks_infer_prompt_tokens_cache_hit_total": 0,
		"gpuworks_infer_requests_success_total":        920,
		"gpuworks_infer_kv_capacity_tokens":            1411072,
		"gpuworks_infer_kv_free_tokens":                1396736,
		"gpuworks_infer_ttft_seconds_sum":              47511.00732,
		"gpuworks_infer_ttft_seconds_count":            920,
		"gpuworks_infer_e2e_seconds_count":             920,
		"gpuworks_infer_ttft_seconds_bucket{le=+Inf}":  920,
		"gpuworks_infer_e2e_seconds_bucket{le=+Inf}":   920,
	}
	for key, value := range want {
		sample, ok := got[key]
		if !ok {
			t.Errorf("missing %s", key)
			continue
		}
		if sample.Value != value {
			t.Errorf("%s = %v, want %v", key, sample.Value, value)
		}
		if sample.Labels["instance"] != "tf-1" || sample.Labels["engine"] != "callable" || sample.Labels["deployment"] != "12" {
			t.Errorf("%s labels = %v", key, sample.Labels)
		}
	}
	// The pool reports size and free rows; the usage ratio every panel reads
	// is derived from them: (1411072-1396736)/1411072.
	usage, ok := got["gpuworks_infer_gpu_cache_usage_percent"]
	if !ok || usage.Value < 0.01015 || usage.Value > 0.01017 {
		t.Fatalf("derived KV usage = %+v", usage)
	}
	// The per-stream ratio and unmapped health gauges stay out.
	for key := range got {
		if strings.Contains(key, "kv_cache_usage_ratio") || strings.Contains(key, "streams") {
			t.Fatalf("unmapped family stored as %s", key)
		}
	}
}

func TestObservationDetectsTheMappingByFamilyPrefix(t *testing.T) {
	detect := []proto.MetricRuleSet{
		{Prefix: "vllm", Rules: []proto.MetricRule{{Source: "vllm:num_requests_running", Series: "gpuworks_infer_requests_running"}}},
		{Prefix: "sglang", Rules: []proto.MetricRule{{Source: "sglang:num_running_reqs", Series: "gpuworks_infer_requests_running"}}},
	}
	in := RunningInstance{UUID: "c-1", Engine: "callable", Observation: &proto.InstanceObservation{MetricDetect: detect}}

	// An adapter wrapping SGLang exposes sglang: families.
	samples := parsePrometheus(strings.NewReader("# TYPE x gauge\nprocess_cpu 3\nsglang:num_running_reqs 4\n"), in, "n", 1, nil, nil)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_running" || samples[0].Value != 4 {
		t.Fatalf("sglang detect = %+v", samples)
	}
	// A vLLM fork that kept vllm: names: the first set in order whose prefix
	// appears wins.
	samples = parsePrometheus(strings.NewReader("vllm:num_requests_running 2\nsglang:num_running_reqs 9\n"), in, "n", 1, nil, nil)
	if len(samples) != 1 || samples[0].Value != 2 {
		t.Fatalf("vllm detect = %+v", samples)
	}
	// TensorFold's own names match no built-in mapping: nothing, not a guess.
	if samples := parsePrometheus(strings.NewReader(tensorfoldScrape(t)), in, "n", 1, nil, nil); len(samples) != 0 {
		t.Fatalf("undeclared tensorfold scrape produced %+v", samples)
	}
	// A "vllm" family that is not "vllm:" does not trigger detection.
	if samples := parsePrometheus(strings.NewReader("vllm_requests 2\n"), in, "n", 1, nil, nil); len(samples) != 0 {
		t.Fatalf("prefix without colon matched: %+v", samples)
	}
}

func TestObservationScalesCountersAndGaugesButNotHistograms(t *testing.T) {
	in := RunningInstance{UUID: "s-1", Observation: &proto.InstanceObservation{MetricRules: []proto.MetricRule{
		{Source: "engine:prefill_ms_total", Series: "gpuworks_infer_prompt_tokens_total", Scale: 0.001},
		{Source: "engine:running", Series: "gpuworks_infer_requests_running"},
		{Source: "engine:ttft_seconds", Series: "gpuworks_infer_ttft_seconds"},
	}}}
	body := "engine:prefill_ms_total 2500\nengine:running 3\nengine:ttft_seconds_bucket{le=\"1\"} 4\nengine:ttft_seconds_sum 2.5\nengine:ttft_seconds_count 4\n"
	got := sampleIndex(parsePrometheus(strings.NewReader(body), in, "n", 1, nil, nil))
	if got["gpuworks_infer_prompt_tokens_total"].Value != 2.5 {
		t.Fatalf("scaled counter = %+v", got["gpuworks_infer_prompt_tokens_total"])
	}
	if got["gpuworks_infer_requests_running"].Value != 3 {
		t.Fatalf("unscaled gauge = %+v", got["gpuworks_infer_requests_running"])
	}
	if got["gpuworks_infer_ttft_seconds_sum"].Value != 2.5 || got["gpuworks_infer_ttft_seconds_bucket{le=1}"].Value != 4 {
		t.Fatalf("histogram = %+v", got)
	}
}

func TestObservationReplacesTheLegacyMap(t *testing.T) {
	// A server that resolved an observation also fills the legacy fields for
	// older agents; this agent must read the observation, not the legacy map.
	in := RunningInstance{UUID: "o-1", Engine: "callable", MetricPrefix: "gpuworks_callable",
		MetricMap:   map[string]string{"old:running": "gpuworks_infer_requests_waiting"},
		Observation: &proto.InstanceObservation{MetricRules: []proto.MetricRule{{Source: "new:running", Series: "gpuworks_infer_requests_running"}}},
	}
	samples := parsePrometheus(strings.NewReader("old:running 1\nnew:running 2\n"), in, "n", 1, nil, nil)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_running" || samples[0].Value != 2 {
		t.Fatalf("samples = %+v", samples)
	}
	// An empty observation (nothing declared, nothing to detect) collects
	// nothing rather than falling back to the legacy prefix.
	in.Observation = &proto.InstanceObservation{}
	if samples := parsePrometheus(strings.NewReader("gpuworks_callable:num_requests_running 5\n"), in, "n", 1, nil, nil); len(samples) != 0 {
		t.Fatalf("empty observation collected %+v", samples)
	}
}

func TestObservationTruncationStillDropsOnlyThePartialLine(t *testing.T) {
	const targetPrefix = "engine:running "
	kept := targetPrefix + "42"
	filler := strings.Repeat("#", (oneMiB+1)-len(kept)-1) + "\n"
	in := RunningInstance{UUID: "t-1", Observation: &proto.InstanceObservation{MetricRules: []proto.MetricRule{
		{Source: "engine:running", Series: "gpuworks_infer_requests_running"},
	}}}
	if samples := parsePrometheus(strings.NewReader(filler+targetPrefix+"4200\n"), in, "n", 1, nil, nil); len(samples) != 0 {
		t.Fatalf("partial final line reported: %+v", samples)
	}
	// A complete line before the cap survives truncation inside unmapped filler.
	head := targetPrefix + "7\n"
	body := head + strings.Repeat("other 1\n", (oneMiB/8)+16)
	samples := parsePrometheus(strings.NewReader(body), in, "n", 1, nil, nil)
	if len(samples) != 1 || samples[0].Value != 7 {
		t.Fatalf("complete earlier sample lost: %+v", samples)
	}
}

func TestInferSamplesScrapesTheObservationEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/prometheus" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("engine:running 6\n"))
	})}
	go srv.Serve(listener)
	defer srv.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	c := New("node-1", nil, func() []RunningInstance {
		return []RunningInstance{{UUID: "inst-1", APIPort: port, Engine: "callable", MetricPath: "/metrics", Observation: &proto.InstanceObservation{
			MetricPath:  "/stats/prometheus",
			MetricRules: []proto.MetricRule{{Source: "engine:running", Series: "gpuworks_infer_requests_running"}},
		}}}
	}, nil, slog.Default())
	samples := c.inferSamples(context.Background(), 123)
	if len(samples) != 1 || samples[0].Value != 6 {
		t.Fatalf("samples = %+v", samples)
	}
}
