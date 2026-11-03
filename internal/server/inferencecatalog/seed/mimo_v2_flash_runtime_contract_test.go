package seed

import (
	"slices"
	"strings"
	"testing"
)

func TestMiMoV2FlashDoesNotAdvertiseA100(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "xiaomimimo/mimo-v2-flash" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" {
				continue
			}
			if spec.GPUs.CC == "8.0" || spec.GPUs.CC == ">=8.0" {
				t.Fatalf("%s advertises unsupported A100 execution: %+v", spec.Name, spec.GPUs)
			}
			if spec.Weights.Quantization != "fp8" {
				t.Fatalf("%s quantization=%q, want fp8", spec.Name, spec.Weights.Quantization)
			}
		}
		_, spec := catalogSpec(t, model.ModelID, "fp8-sglang-nvidia-hopper")
		if spec.GPUs.CC != ">=9.0" {
			t.Fatalf("cc=%q, want Hopper+", spec.GPUs.CC)
		}
		args := strings.Join(spec.Args, " ")
		for _, want := range []string{
			"--dp-size 2",
			"--enable-dp-attention",
			"--page-size 1",
			"--context-length 262144",
			"--reasoning-parser qwen3",
			"--tool-call-parser mimo",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
			}
		}
		if !slices.Contains(spec.Args, "--trust-remote-code") {
			t.Errorf("launch arguments must preserve the Hugging Face chat template: %v", spec.Args)
		}
		return
	}
	t.Fatal("MiMo-V2-Flash model not found")
}
