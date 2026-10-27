package collector

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSGLangMetricsAcrossObservationAndLegacyFallback(t *testing.T) {
	body, err := os.ReadFile("testdata/sglang-v0.5.3.metrics")
	if err != nil {
		t.Fatal(err)
	}
	var rules []proto.MetricRule
	metricMap := map[string]string{}
	for _, rule := range inferencemetrics.EngineRules(engines.SGLang) {
		definition, _ := inferencemetrics.Lookup(rule.Metric)
		rules = append(rules, proto.MetricRule{Source: rule.Source, Series: definition.Series(), Scale: rule.Scale})
		metricMap[rule.Source] = definition.Series()
	}
	for _, tc := range []struct {
		name string
		in   RunningInstance
	}{
		{"engine fallback", RunningInstance{Engine: engines.SGLang}},
		{"prefix fallback", RunningInstance{MetricPrefix: engines.SGLang}},
		{"legacy map", RunningInstance{Engine: engines.SGLang, MetricMap: metricMap}},
		{"observation", RunningInstance{Engine: engines.SGLang, Observation: &proto.InstanceObservation{MetricRules: rules}}},
		{"detected callable", RunningInstance{Engine: engines.Callable, Observation: &proto.InstanceObservation{MetricDetect: []proto.MetricRuleSet{{Prefix: engines.SGLang, Rules: rules}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.UUID, in.DeploymentID = "sg-1", 12
			samples := parsePrometheus(strings.NewReader(string(body)), in, "node-1", 123, nil, nil)
			got := sampleIndex(samples)
			want := map[string]float64{"gpuworks_infer_requests_running": 4, "gpuworks_infer_requests_waiting": 2, "gpuworks_infer_gpu_cache_usage_percent": 0.75}
			if len(samples) != len(want) {
				t.Fatalf("samples = %+v", samples)
			}
			for name, value := range want {
				sample, ok := got[name]
				if !ok || sample.Value != value {
					t.Errorf("%s = %+v, want %v", name, sample, value)
				}
				engine := in.Engine
				if tc.name == "prefix fallback" {
					engine = engines.VLLM // Existing default label when Engine is absent.
				}
				wantLabels := map[string]string{"node": "node-1", "instance": "sg-1", "engine": engine, "deployment": "12"}
				if sample.TimeMS != 123 || !reflect.DeepEqual(sample.Labels, wantLabels) {
					t.Errorf("sample metadata = %+v", sample)
				}
			}
		})
	}
}

func TestSGLangFallbackPreservesAbsentAndZeroMetrics(t *testing.T) {
	in := RunningInstance{UUID: "sg-1", Engine: engines.SGLang}
	for _, body := range []string{"", "process_cpu_seconds_total 1\n", "vllm:num_requests_running 3\n"} {
		if got := parsePrometheus(strings.NewReader(body), in, "n", 1, nil, nil); len(got) != 0 {
			t.Fatalf("absent metrics synthesized for %q: %+v", body, got)
		}
	}
	got := parsePrometheus(strings.NewReader("sglang:num_running_reqs 0\n"), in, "n", 1, nil, nil)
	if len(got) != 1 || got[0].Metric != "gpuworks_infer_requests_running" || got[0].Value != 0 {
		t.Fatalf("reported zero = %+v", got)
	}
}

func TestSGLangFallbackKeepsGaugeDeduplication(t *testing.T) {
	// Gauge label combinations and compatibility aliases must yield one sample,
	// not be summed like counters. Preserve the collector's first-value policy.
	body := `sglang:num_running_reqs{model_name="qwen",tp_rank="0"} 4
sglang:num_running_reqs{model_name="qwen",tp_rank="1"} 4
sglang:num_requests_running 4
sglang:token_usage{model_name="qwen",tp_rank="0"} 0.75
sglang:token_usage{model_name="qwen",tp_rank="1"} 0.75
`
	got := parsePrometheus(strings.NewReader(body), RunningInstance{Engine: engines.SGLang}, "n", 1, nil, nil)
	if len(got) != 2 || got[0].Value != 4 || got[1].Value != 0.75 {
		t.Fatalf("deduplicated gauges = %+v", got)
	}
}
