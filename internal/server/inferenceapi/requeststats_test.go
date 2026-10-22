package inferenceapi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestRequestStatsYAMLRoundTripsAndNormalizes(t *testing.T) {
	contract := decodeContractYAML(t, `
operations:
- type: openai.chat
request_stats:
  ' tensorfold.prefill_s ': prefill_seconds
  timings.predicted_ms: {to: decode_seconds, scale: 0.001}
`)
	normalized, err := NormalizeContract(contract)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := normalized.RequestStats["tensorfold.prefill_s"]; got.To != inferencemetrics.RequestPrefillSeconds {
		t.Fatalf("trimmed path missing: %+v", normalized.RequestStats)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	// The wire form keeps a bare name for an unscaled entry and {to, scale}
	// for a scaled one, the same two shapes api.metrics uses.
	want := `"request_stats":{"tensorfold.prefill_s":"prefill_seconds","timings.predicted_ms":{"to":"decode_seconds","scale":0.001}}`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("json = %s, want it to contain %s", raw, want)
	}
	var back Contract
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.RequestStats["timings.predicted_ms"].Scale != 0.001 {
		t.Fatalf("round trip lost the scale: %+v", back.RequestStats)
	}
}

func TestRequestStatsReportStableCodes(t *testing.T) {
	cases := map[string]struct {
		stats MetricMap
		code  string
	}{
		"path":     {MetricMap{"a..b": {To: "queue_seconds"}}, messages.ErrorCatalogRequestStatsPathInvalid},
		"stat":     {MetricMap{"a.b": {To: "ttft_seconds"}}, messages.ErrorCatalogRequestStatsUnknownStat},
		"scale":    {MetricMap{"a.b": {To: "queue_seconds", Scale: -2}}, messages.ErrorCatalogRequestStatsNegativeScale},
		"trimDupe": {MetricMap{"a.b": {To: "queue_seconds"}, " a.b": {To: "decode_seconds"}}, messages.ErrorCatalogRequestStatsDuplicatePath},
	}
	for name, c := range cases {
		_, err := NormalizeContract(Contract{Operations: chatOperation, RequestStats: c.stats})
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != c.code {
			t.Errorf("%s: err = %v, want code %s", name, err, c.code)
		}
	}
}

func TestResolveRequestStatsOverlaysTemplateOnEngine(t *testing.T) {
	rules := ResolveRequestStats("vllm", MetricMap{"metrics.queue_time_ms": {To: "queue_seconds", Scale: 0.002}, "x.decode": {To: "decode_seconds"}})
	byPath := map[string]inferencemetrics.RequestStatRule{}
	for _, rule := range rules {
		byPath[rule.Path] = rule
	}
	if byPath["metrics.queue_time_ms"].Scale != 0.002 {
		t.Fatalf("template entry must win: %+v", byPath["metrics.queue_time_ms"])
	}
	if _, ok := byPath["metrics.generation_time_ms"]; !ok {
		t.Fatal("engine rules must remain")
	}
	if _, ok := byPath["x.decode"]; !ok {
		t.Fatal("template-only rule missing")
	}
	if len(ResolveRequestStats("callable", nil)) != 0 {
		t.Fatal("an undeclared callable has no rules")
	}
}
