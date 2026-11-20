package api

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The template detail carries a spec's api.metrics on the wire as it was
// declared: a short-form target stays a string, a scaled one an object, so the
// editor writes back exactly what it read. A spec without one has no key.
func TestInferenceTemplateViewCarriesAPIMetricsOnTheWire(t *testing.T) {
	declared := `{"operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions"}],` +
		`"metrics":{"endpoint":"/stats","inherit":"sglang","map":{"adapter:ms_total":{"to":"prompt_tokens_total","scale":0.001},"tensorfold:requests_running":"requests_running"}}}`
	view := inferenceTemplateView(store.InferenceModel{ModelID: "acme/metrics", Source: "user", Specs: []store.InferenceSpecRow{
		{Name: "declared", Source: "user", Engine: deploy.EngineCallable, APIContract: declared},
		{Name: "plain", Source: "user", Engine: deploy.EngineCallable, APIContract: `{"operations":[{"type":"openai.chat"}]}`},
	}})
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Specs []struct {
			Name string                     `json:"name"`
			API  map[string]json.RawMessage `json:"api"`
		} `json:"specs"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Specs) != 2 {
		t.Fatalf("specs = %s", encoded)
	}
	want := `{"endpoint":"/stats","inherit":"sglang","map":{"adapter:ms_total":{"to":"prompt_tokens_total","scale":0.001},"tensorfold:requests_running":"requests_running"}}`
	for _, spec := range wire.Specs {
		metrics, present := spec.API["metrics"]
		switch spec.Name {
		case "declared":
			if string(metrics) != want {
				t.Fatalf("api.metrics = %s\nwant          %s", metrics, want)
			}
		case "plain":
			if present {
				t.Fatalf("a spec without metrics has api.metrics = %s", metrics)
			}
			if !strings.Contains(string(spec.API["operations"]), "openai.chat") {
				t.Fatalf("api = %v", spec.API)
			}
		}
	}
}

func TestInferenceTemplateViewCarriesTheMetricsFormat(t *testing.T) {
	declared := `{"operations":[{"type":"openai.chat"}],"metrics":{"format":"json","map":{"totals.output_tokens":"generation_tokens_total"}}}`
	view := inferenceTemplateView(store.InferenceModel{ModelID: "acme/json", Source: "user", Specs: []store.InferenceSpecRow{
		{Name: "json", Source: "user", Engine: deploy.EngineCallable, APIContract: declared},
	}})
	encoded, err := json.Marshal(view.Specs[0].API.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"format":"json","map":{"totals.output_tokens":"generation_tokens_total"}}`; string(encoded) != want {
		t.Fatalf("api.metrics = %s\nwant          %s", encoded, want)
	}
}

// A spec that serves its metrics on a port of its own says so on the template
// detail: api.metrics.port is part of what the spec declares, and a reader that
// does not see it would show a spec that reserves a second port as one that
// does not. A spec on the API port has no key.
func TestInferenceTemplateViewCarriesTheMetricsPort(t *testing.T) {
	view := inferenceTemplateView(store.InferenceModel{ModelID: "acme/metrics-port", Source: "user", Specs: []store.InferenceSpecRow{
		{Name: "separate", Source: "user", Engine: deploy.EngineCallable, APIContract: `{"operations":[{"type":"openai.chat"}],"metrics":{"endpoint":"/stats","port":"separate"}}`},
		{Name: "api", Source: "user", Engine: deploy.EngineCallable, APIContract: `{"operations":[{"type":"openai.chat"}],"metrics":{"endpoint":"/stats"}}`},
	}})
	byName := map[string]string{}
	for _, spec := range view.Specs {
		encoded, err := json.Marshal(spec.API.Metrics)
		if err != nil {
			t.Fatal(err)
		}
		byName[spec.Name] = string(encoded)
	}
	if byName["separate"] != `{"endpoint":"/stats","port":"separate"}` || byName["api"] != `{"endpoint":"/stats"}` {
		t.Fatalf("api.metrics on the wire = %v", byName)
	}
}
