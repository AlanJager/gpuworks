package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelDTODoesNotExposeEngineSupportClaims(t *testing.T) {
	st, err := store.Open("file:model_mapper_engine_fields_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	d := (&apiHandlers{d: Deps{Store: st}}).modelDTO(&store.Model{Name: "model"}, false)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"vllm_support", "engine_support"} {
		if _, ok := wire[key]; ok {
			t.Fatalf("model DTO exposes retired field %q: %s", key, raw)
		}
	}
}

func TestRedactAcquisitionSecrets(t *testing.T) {
	v := map[string]any{"acquisition": map[string]any{"network": map[string]any{"proxy": "http://user:secret@proxy.example:3128"}}}
	redactAcquisitionSecrets(v)
	proxy := v["acquisition"].(map[string]any)["network"].(map[string]any)["proxy"]
	if proxy != "http://REDACTED@proxy.example:3128" {
		t.Fatalf("proxy=%v", proxy)
	}
}

func TestModelDTOProjectsEditableInferenceConfig(t *testing.T) {
	st, err := store.Open("file:model_mapper_inference_config_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	d := h.modelDTO(&store.Model{Operations: `[{"type":"openai.embeddings"}]`, InputModalities: `["text"]`}, false)
	if len(d.Operations) != 1 || d.Operations[0].Type != "openai.embeddings" {
		t.Fatalf("operations = %+v", d.Operations)
	}
	if len(d.InputModalities) != 1 || d.InputModalities[0] != "text" {
		t.Fatalf("input modalities = %v", d.InputModalities)
	}
}

func TestModelDTOHonorsExplicitlyClearedOperationsForCatalogModel(t *testing.T) {
	st, err := store.Open("file:model_mapper_cleared_operations_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/catalog", Source: "user", InputModalities: `["text"]`,
		Specs: []store.InferenceSpecRow{{Name: "default", Engine: "vllm", Image: "busybox:1.36", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`, APIContract: `{"operations":[{"type":"openai.chat"}]}`}},
	}}, false); err != nil {
		t.Fatal(err)
	}
	d := (&apiHandlers{d: Deps{Store: st}}).modelDTO(&store.Model{Source: "huggingface", SourceID: "acme/catalog", Operations: "[]", InputModalities: "[]"}, false)
	if d.Operations == nil || len(d.Operations) != 0 || d.InputModalities == nil || len(d.InputModalities) != 0 {
		t.Fatalf("explicit clear was replaced by catalog fallback: operations=%+v modalities=%v", d.Operations, d.InputModalities)
	}
}
