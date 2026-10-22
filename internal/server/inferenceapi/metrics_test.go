package inferenceapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

var chatOperation = []Operation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}

func decodeContractYAML(t *testing.T, text string) Contract {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(text))
	decoder.KnownFields(true)
	var contract Contract
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return contract
}

func TestMetricsYAMLAcceptsBothTargetFormsAndRoundTrips(t *testing.T) {
	contract := decodeContractYAML(t, `
operations:
- type: openai.chat
metrics:
  endpoint: /stats/prometheus
  inherit: sglang
  map:
    'tensorfold:time_to_first_token_seconds': ttft_seconds
    'engine:prefill_ms_total': {to: prompt_tokens_total, scale: 0.001}
`)
	if contract.Metrics == nil || contract.Metrics.Endpoint != "/stats/prometheus" || contract.Metrics.Inherit != "sglang" {
		t.Fatalf("metrics = %+v", contract.Metrics)
	}
	if got := contract.Metrics.Map["tensorfold:time_to_first_token_seconds"]; got != (MetricTarget{To: "ttft_seconds"}) {
		t.Fatalf("short form = %+v", got)
	}
	if got := contract.Metrics.Map["engine:prefill_ms_total"]; got != (MetricTarget{To: "prompt_tokens_total", Scale: 0.001}) {
		t.Fatalf("long form = %+v", got)
	}
	normalized, err := NormalizeContract(contract)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	// The stored column is JSON: the short form stays a string, the long form
	// an object, and decoding it gives back the same declaration.
	encoded, err := json.Marshal(normalized.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"endpoint":"/stats/prometheus","inherit":"sglang","map":{"engine:prefill_ms_total":{"to":"prompt_tokens_total","scale":0.001},"tensorfold:time_to_first_token_seconds":"ttft_seconds"}}`
	if string(encoded) != want {
		t.Fatalf("json = %s\nwant  %s", encoded, want)
	}
	var back Metrics
	if err := json.Unmarshal(encoded, &back); err != nil || back.Map["engine:prefill_ms_total"].Scale != 0.001 {
		t.Fatalf("json round trip = %+v, %v", back, err)
	}
	reYAML, err := yaml.Marshal(normalized)
	if err != nil || !strings.Contains(string(reYAML), "tensorfold:time_to_first_token_seconds: ttft_seconds") || !strings.Contains(string(reYAML), "scale: 0.001") {
		t.Fatalf("yaml = %s, %v", reYAML, err)
	}
}

func TestMetricsRejectsUnknownKeysInEitherForm(t *testing.T) {
	decoder := yaml.NewDecoder(strings.NewReader("operations: [{type: openai.chat}]\nmetrics:\n  map:\n    x: {to: requests_running, factor: 2}\n"))
	decoder.KnownFields(true)
	var contract Contract
	if err := decoder.Decode(&contract); err == nil || !strings.Contains(err.Error(), "factor") {
		t.Fatalf("unknown long-form key accepted: %v", err)
	}
	decoder = yaml.NewDecoder(strings.NewReader("operations: [{type: openai.chat}]\nmetrics:\n  path: /metrics\n"))
	decoder.KnownFields(true)
	if err := decoder.Decode(&contract); err == nil {
		t.Fatal("unknown metrics key accepted")
	}
	decoder = yaml.NewDecoder(strings.NewReader("operations: [{type: openai.chat}]\nmetrics:\n  map:\n    x: [requests_running]\n"))
	decoder.KnownFields(true)
	if err := decoder.Decode(&contract); err == nil {
		t.Fatal("a sequence target accepted")
	}
	var target MetricTarget
	if err := json.Unmarshal([]byte(`{"to":"requests_running","factor":2}`), &target); err == nil {
		t.Fatal("unknown JSON long-form key accepted")
	}
	if err := json.Unmarshal([]byte(`3`), &target); err == nil {
		t.Fatal("a number target accepted")
	}
}

func TestNormalizeMetricsReportsStableCodes(t *testing.T) {
	cases := []struct {
		name    string
		metrics Metrics
		code    string
	}{
		{"relative endpoint", Metrics{Endpoint: "metrics"}, messages.ErrorCatalogMetricsEndpointInvalid},
		{"unknown inherit", Metrics{Inherit: "tensorfold"}, messages.ErrorCatalogMetricsInheritUnknown},
		{"callable cannot be inherited", Metrics{Inherit: "callable"}, messages.ErrorCatalogMetricsInheritUnknown},
		{"bad selector", Metrics{Map: MetricMap{`x{tier=1}`: {To: "requests_running"}}}, messages.ErrorCatalogMetricsSourceInvalid},
		{"unknown metric", Metrics{Map: MetricMap{"x": {To: "ttft_second"}}}, messages.ErrorCatalogMetricsUnknownMetric},
		{"histogram selector", Metrics{Map: MetricMap{`x{a="b"}`: {To: "ttft_seconds"}}}, messages.ErrorCatalogMetricsHistogramSelector},
		{"histogram scale", Metrics{Map: MetricMap{"x": {To: "ttft_seconds", Scale: 2}}}, messages.ErrorCatalogMetricsHistogramScale},
		{"negative scale", Metrics{Map: MetricMap{"x": {To: "requests_running", Scale: -2}}}, messages.ErrorCatalogMetricsNegativeScale},
		{"duplicate after trimming", Metrics{Map: MetricMap{"x": {To: "requests_running"}, " x ": {To: "requests_waiting"}}}, messages.ErrorCatalogMetricsDuplicateSource},
		// A template entry is checked against the mapping it inherits: a
		// _count line of an inherited histogram family would be dead.
		{"shadowed by inherited family", Metrics{Inherit: "vllm", Map: MetricMap{"vllm:time_to_first_token_seconds_count": {To: "requests_success_total"}}}, messages.ErrorCatalogMetricsShadowedHistogram},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metrics := tc.metrics
			_, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &metrics})
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}

func TestNormalizeMetricsTrimsAndKeepsAValidDeclaration(t *testing.T) {
	normalized, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{
		Endpoint: " /metrics ", Inherit: " SGLang ",
		Map: MetricMap{" sglang:token_usage ": {To: " gpu_cache_usage_percent "}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	metrics := normalized.Metrics
	if metrics.Endpoint != "/metrics" || metrics.Inherit != "sglang" {
		t.Fatalf("metrics = %+v", metrics)
	}
	if got := metrics.Map["sglang:token_usage"]; got.To != "gpu_cache_usage_percent" {
		t.Fatalf("map = %+v", metrics.Map)
	}
	rules := metrics.Rules()
	if len(rules) != 1 || rules[0].Source != "sglang:token_usage" || rules[0].Metric != "gpu_cache_usage_percent" {
		t.Fatalf("rules = %+v", rules)
	}
	// An empty declaration is allowed and means the defaults.
	if _, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{}}); err != nil {
		t.Fatalf("empty metrics refused: %v", err)
	}
	// A contract without metrics stays without.
	plain, err := NormalizeContract(Contract{Operations: chatOperation})
	if err != nil || plain.Metrics != nil {
		t.Fatalf("plain contract = %+v, %v", plain, err)
	}
}

func TestMetricsJSONFormat(t *testing.T) {
	contract := decodeContractYAML(t, `
operations:
- type: openai.chat
metrics:
  format: JSON
  map:
    'totals.output_tokens': generation_tokens_total
    'live.queued': requests_waiting
    'live.state!=idle': requests_running
`)
	normalized, err := NormalizeContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(normalized.Metrics)
	if want := `{"format":"json","map":{"live.queued":"requests_waiting","live.state!=idle":"requests_running","totals.output_tokens":"generation_tokens_total"}}`; string(encoded) != want {
		t.Fatalf("json = %s\nwant  %s", encoded, want)
	}
	// Prometheus, the default, is stored as no format.
	plain, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{Format: "Prometheus", Map: MetricMap{"x": {To: "requests_running"}}}})
	if err != nil || plain.Metrics.Format != "" {
		t.Fatalf("prometheus format = %q, %v", plain.Metrics.Format, err)
	}
	for _, tc := range []struct {
		name    string
		metrics Metrics
		code    string
	}{
		{"unknown format", Metrics{Format: "openmetrics"}, messages.ErrorCatalogMetricsFormatInvalid},
		{"json cannot inherit", Metrics{Format: "json", Inherit: "sglang"}, messages.ErrorCatalogMetricsJSONInherit},
		{"json path", Metrics{Format: "json", Map: MetricMap{"vllm:num_requests_running": {To: "requests_running"}}}, messages.ErrorCatalogMetricsJSONPathInvalid},
		{"json histogram", Metrics{Format: "json", Map: MetricMap{"timings.ttft": {To: "ttft_seconds"}}}, messages.ErrorCatalogMetricsJSONHistogram},
		{"json comparison into a counter", Metrics{Format: "json", Map: MetricMap{"live.state!=idle": {To: "requests_success_total"}}}, messages.ErrorCatalogMetricsJSONCompareCounter},
		{"json unknown metric", Metrics{Format: "json", Map: MetricMap{"live.tok_s": {To: "tok_s"}}}, messages.ErrorCatalogMetricsUnknownMetric},
	} {
		metrics := tc.metrics
		_, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &metrics})
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != tc.code {
			t.Fatalf("%s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
}
