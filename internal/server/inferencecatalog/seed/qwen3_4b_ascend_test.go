package seed

import "testing"

func TestQwen3FourBHasValidatedA3Profile(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen3-4b" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-a3-qwen3-4b" {
				continue
			}
			found = true
			if spec.Engine != "sglang" ||
				spec.Image != "gpuworks/gpuworks-runner:cann9.0-a3-sglang0.5.18-r2" ||
				spec.WeightBytes != 8060927256 ||
				!containsArgumentPair(spec.Args, "--device", "npu") ||
				!containsArgumentPair(spec.Args, "--attention-backend", "ascend") {
				t.Fatalf("A3 profile does not preserve the measured runner contract: %+v", spec)
			}
		}
	}
	if !found {
		t.Fatal("qwen/qwen3-4b has no validated A3 profile")
	}
}
