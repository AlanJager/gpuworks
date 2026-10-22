package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func ruleFor(rules []proto.MetricRule, source string) (proto.MetricRule, bool) {
	for _, rule := range rules {
		if rule.Source == source {
			return rule, true
		}
	}
	return proto.MetricRule{}, false
}

func TestResolveObservationUsesTheEngineMappingWhenNothingIsDeclared(t *testing.T) {
	for _, engine := range []string{EngineVLLM, ""} {
		observation := ResolveObservation(engine, nil)
		rule, ok := ruleFor(observation.Instance.MetricRules, "vllm:num_requests_running")
		if !ok || rule.Series != "gpuworks_infer_requests_running" {
			t.Fatalf("engine %q rules = %+v", engine, observation.Instance.MetricRules)
		}
		if observation.Instance.MetricPath != "/metrics" || len(observation.Instance.MetricDetect) != 0 {
			t.Fatalf("engine %q observation = %+v", engine, observation.Instance)
		}
		// An older agent reads the same mapping from the legacy fields.
		if observation.LegacyPath != "/metrics" || observation.LegacyPrefix != EngineVLLM ||
			len(observation.LegacyMap) != len(EngineMetricMap(EngineVLLM)) ||
			observation.LegacyMap["vllm:num_requests_running"] != "gpuworks_infer_requests_running" {
			t.Fatalf("engine %q legacy = %q %q %d", engine, observation.LegacyPath, observation.LegacyPrefix, len(observation.LegacyMap))
		}
	}
	sglang := ResolveObservation(EngineSGLang, nil)
	if rule, ok := ruleFor(sglang.Instance.MetricRules, "sglang:token_usage"); !ok || rule.Series != "gpuworks_infer_gpu_cache_usage_percent" {
		t.Fatalf("sglang rules = %+v", sglang.Instance.MetricRules)
	}
}

func TestResolveObservationDetectsForAnUndeclaredCallable(t *testing.T) {
	observation := ResolveObservation(EngineCallable, nil)
	if len(observation.Instance.MetricRules) != 0 || len(observation.Instance.MetricDetect) != 2 {
		t.Fatalf("callable observation = %+v", observation.Instance)
	}
	if observation.Instance.MetricDetect[0].Prefix != EngineVLLM || observation.Instance.MetricDetect[1].Prefix != EngineSGLang {
		t.Fatalf("detect order = %+v", observation.Instance.MetricDetect)
	}
	// An older agent keeps the prefix fallback it always had for callable.
	if observation.LegacyPrefix != "gpuworks_callable" || observation.LegacyMap != nil || observation.LegacyPath != "/metrics" {
		t.Fatalf("legacy = %q %v %q", observation.LegacyPrefix, observation.LegacyMap, observation.LegacyPath)
	}
}

func TestResolveObservationLaysTheTemplateOverItsInheritedMapping(t *testing.T) {
	declared := &inferenceapi.Metrics{
		Endpoint: "/stats",
		Inherit:  EngineSGLang,
		Map: inferenceapi.MetricMap{
			// Overrides the inherited target for the same family.
			"sglang:num_running_reqs": {To: "requests_waiting"},
			"adapter:ms_total":        {To: "prompt_tokens_total", Scale: 0.001},
			"adapter:ttft":            {To: "ttft_seconds", Scale: 1},
		},
	}
	observation := ResolveObservation(EngineCallable, declared)
	if observation.Instance.MetricPath != "/stats" || observation.LegacyPath != "/stats" {
		t.Fatalf("path = %q / %q", observation.Instance.MetricPath, observation.LegacyPath)
	}
	if len(observation.Instance.MetricDetect) != 0 {
		t.Fatal("a declared mapping still detects")
	}
	if rule, _ := ruleFor(observation.Instance.MetricRules, "sglang:num_running_reqs"); rule.Series != "gpuworks_infer_requests_waiting" {
		t.Fatalf("template entry lost to the inherited one: %+v", rule)
	}
	if rule, ok := ruleFor(observation.Instance.MetricRules, "sglang:num_queue_reqs"); !ok || rule.Series != "gpuworks_infer_requests_waiting" {
		t.Fatalf("inherited entry missing: %+v", observation.Instance.MetricRules)
	}
	scaled, _ := ruleFor(observation.Instance.MetricRules, "adapter:ms_total")
	if scaled.Scale != 0.001 || scaled.Series != "gpuworks_infer_prompt_tokens_total" {
		t.Fatalf("scaled rule = %+v", scaled)
	}
	if unit, _ := ruleFor(observation.Instance.MetricRules, "adapter:ttft"); unit.Scale != 0 {
		t.Fatalf("a scale of one is written as no scale: %+v", unit)
	}
	// The legacy map cannot scale, so an older agent does not get the scaled
	// family at all rather than storing it in the wrong unit.
	if _, present := observation.LegacyMap["adapter:ms_total"]; present {
		t.Fatal("scaled rule reached the legacy map")
	}
	if observation.LegacyMap["adapter:ttft"] != "gpuworks_infer_ttft_seconds" {
		t.Fatalf("legacy map = %v", observation.LegacyMap)
	}
	// Rules come out in source order, so an unchanged template resolves to an
	// identical observation and the in-place refresh writes nothing.
	again := ResolveObservation(EngineCallable, declared)
	for i := range again.Instance.MetricRules {
		if again.Instance.MetricRules[i] != observation.Instance.MetricRules[i] {
			t.Fatalf("resolution is not deterministic at %d", i)
		}
		if i > 0 && observation.Instance.MetricRules[i-1].Source >= observation.Instance.MetricRules[i].Source {
			t.Fatalf("rules not sorted at %d", i)
		}
	}
}

func TestObservationApplyStampsAllFourFields(t *testing.T) {
	spec := proto.InstanceDesiredState{UUID: "u", MetricPath: "/old", MetricPrefix: "old", MetricMap: map[string]string{"a": "b"}}
	ResolveObservation(EngineCallable, &inferenceapi.Metrics{Map: inferenceapi.MetricMap{"x": {To: "requests_running"}}}).Apply(&spec)
	if spec.MetricPath != "/metrics" || spec.MetricPrefix != "gpuworks_callable" || spec.MetricMap["x"] != "gpuworks_infer_requests_running" || len(spec.MetricMap) != 1 {
		t.Fatalf("legacy fields = %q %q %v", spec.MetricPath, spec.MetricPrefix, spec.MetricMap)
	}
	if spec.Observation == nil || len(spec.Observation.MetricRules) != 1 || spec.UUID != "u" {
		t.Fatalf("observation = %+v", spec.Observation)
	}
}

func TestEngineMetricMapIsTheStoredSeriesViewOfTheBuiltInRules(t *testing.T) {
	vllm := EngineMetricMap(EngineVLLM)
	if vllm[`vllm:prompt_tokens_by_source_total{source="local_cache_hit"}`] != "gpuworks_infer_prompt_tokens_cache_hit_total" ||
		vllm["vllm:inter_token_latency_seconds"] != "gpuworks_infer_tpot_seconds" || len(vllm) < 30 {
		t.Fatalf("vllm map = %v", vllm)
	}
	if EngineMetricMap(EngineCallable) != nil {
		t.Fatal("callable has a built-in map")
	}
}

// A JSON endpoint gets only the template's own paths: no engine mapping,
// no detection, and no legacy map an older agent would misread.
func TestResolveObservationForAJSONEndpoint(t *testing.T) {
	observation := ResolveObservation(EngineCallable, &inferenceapi.Metrics{Format: "json", Map: inferenceapi.MetricMap{
		"totals.output_tokens": {To: "generation_tokens_total"},
		"live.queued":          {To: "requests_waiting"},
	}})
	got := observation.Instance
	if got.MetricFormat != "json" || got.MetricPath != "/metrics" || len(got.MetricDetect) != 0 || len(got.MetricRules) != 2 {
		t.Fatalf("observation = %+v", got)
	}
	if got.MetricRules[0].Source != "live.queued" || got.MetricRules[0].Series != "gpuworks_infer_requests_waiting" {
		t.Fatalf("rules = %+v", got.MetricRules)
	}
	if observation.LegacyMap != nil {
		t.Fatalf("legacy map = %v", observation.LegacyMap)
	}
	// Even for a vllm engine, a JSON declaration does not inherit vLLM's rules.
	if vllm := ResolveObservation(EngineVLLM, &inferenceapi.Metrics{Format: "json", Map: inferenceapi.MetricMap{"a": {To: "requests_running"}}}); len(vllm.Instance.MetricRules) != 1 {
		t.Fatalf("vllm json rules = %+v", vllm.Instance.MetricRules)
	}
}
