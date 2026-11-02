package seed

import "testing"

func TestPaddleOCRVL16HasValidatedA3Profile(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "paddlepaddle/paddleocr-vl-1.6" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-vllm-ascend-a3-paddleocr16" {
				continue
			}
			found = true
			if spec.Engine != "vllm" ||
				spec.Image != "gpuworks/gpuworks-runner:cann9.1-a3-vllm0.26.0rc1-r2" ||
				spec.WeightBytes != 1930462439 ||
				!containsArgumentPair(spec.Args, "--dtype", "bfloat16") ||
				!containsArgumentPair(spec.Args, "--limit-mm-per-prompt", `{"image": 1}`) {
				t.Fatalf("A3 profile does not preserve the measured OCR contract: %+v", spec)
			}
		}
	}
	if !found {
		t.Fatal("paddlepaddle/paddleocr-vl-1.6 has no validated A3 profile")
	}
}
