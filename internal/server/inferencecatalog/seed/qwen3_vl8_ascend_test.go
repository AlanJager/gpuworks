package seed

import "testing"

func TestQwen3VL8A3UsesMeasuredSingleDieContext(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "qwen/qwen3-vl-8b-instruct" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-ascend-a3-4eac0a8b" {
				continue
			}
			found = true
			if !containsArgumentPair(spec.Args, "--max-model-len", "196608") {
				t.Errorf("A3 profile lacks the measured single-die context limit: %q", spec.Args)
			}
		}
	}
	if !found {
		t.Fatal("Qwen3-VL-8B A3 profile is missing")
	}
}
