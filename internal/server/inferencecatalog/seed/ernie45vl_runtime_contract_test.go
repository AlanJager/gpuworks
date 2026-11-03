package seed

import (
	"slices"
	"testing"
)

func TestERNIE45VLUsesDynamicModelRunner(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "baidu/ernie-4.5-vl-28b-a3b-pt" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" {
				continue
			}
			if spec.Image != "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4" {
				t.Fatalf("%s image=%q, want the runner with decord/moviepy support", spec.Name, spec.Image)
			}
			if spec.Name == "bf16-default-vllm-nvidia-a100-r4" {
				index := slices.Index(spec.Args, "--max-model-len")
				if spec.GPUs.CC != "8.0" || index < 0 || index+1 >= len(spec.Args) || spec.Args[index+1] != "32768" {
					t.Fatalf("A100 spec cc=%q args=%v, want cc=8.0 and --max-model-len 32768", spec.GPUs.CC, spec.Args)
				}
				return
			}
		}
		t.Fatal("ERNIE-4.5-VL A100 NVIDIA vLLM profile is missing")
	}
	t.Fatal("ERNIE-4.5-VL model is missing")
}
