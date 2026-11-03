package seed

import "testing"

func TestStep35DoesNotExposeGGUFOnlyWeightsAsVLLM(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "stepfun-ai/step-3.5-flash" {
			continue
		}
		hasTransformersVLLM := false
		for _, spec := range model.Specs {
			if spec.Engine != "vllm" {
				continue
			}
			if spec.Name == "int4-vllm-nvidia-general" || spec.Name == "int8-vllm-nvidia-general" {
				t.Errorf("Step-3.5 GGUF shards are exposed as %s", spec.Name)
			}
			for _, source := range spec.Weights.Sources {
				if source.RepoID == "stepfun-ai/Step-3.5-Flash-INT8" {
					t.Errorf("%s points vLLM at split-GGUF repository %s", spec.Name, source.RepoID)
				}
				if source.RepoID == "stepfun-ai/Step-3.5-Flash" {
					hasTransformersVLLM = true
				}
			}
		}
		if !hasTransformersVLLM {
			t.Error("Step-3.5 lost its Transformers-weight vLLM profile")
		}
		return
	}
	t.Fatal("Step-3.5 model is missing")
}
