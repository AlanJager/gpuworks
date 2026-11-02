package seed

import "testing"

func TestQwen25OnePointFiveBHasValidatedA3Profile(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen2.5-1.5b" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-a3" {
				continue
			}
			found = true
			if spec.Engine != "sglang" ||
				spec.Image != "gpuworks/gpuworks-runner:cann9.0-a3-sglang0.5.18-r2" ||
				spec.WeightBytes != 3087467144 ||
				!containsArgumentPair(spec.Args, "--attention-backend", "ascend") ||
				!containsArgumentPair(spec.Args, "--dtype", "bfloat16") {
				t.Fatalf("A3 profile does not preserve the measured runner contract: %+v", spec)
			}
		}
	}
	if !found {
		t.Fatal("qwen/qwen2.5-1.5b has no validated bf16-sglang-ascend-a3 profile")
	}
}
