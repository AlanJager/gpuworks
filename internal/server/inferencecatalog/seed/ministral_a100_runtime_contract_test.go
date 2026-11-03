package seed

import (
	"strings"
	"testing"
)

func TestMinistralHasVerifiedA100Profiles(t *testing.T) {
	tests := []struct {
		modelID  string
		specName string
	}{
		{
			modelID:  "mistralai/ministral-3-8b-instruct-2512",
			specName: "fp8-8b-vllm-nvidia-a100",
		},
		{
			modelID:  "mistralai/ministral-3-14b-instruct-2512",
			specName: "fp8-14b-vllm-nvidia-a100",
		},
	}
	for _, test := range tests {
		t.Run(test.specName, func(t *testing.T) {
			_, spec := catalogSpec(t, test.modelID, test.specName)
			if spec.Image != "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2" {
				t.Fatalf("image=%q, want pinned FP8-capable runner", spec.Image)
			}
			if spec.GPUs.Vendor != "nvidia" || spec.GPUs.CC != "8.0" {
				t.Fatalf("gpus=%+v, want A100 CC 8.0", spec.GPUs)
			}
			if spec.Weights.Quantization != "fp8" {
				t.Fatalf("quantization=%q, want fp8", spec.Weights.Quantization)
			}
			if test.modelID == "mistralai/ministral-3-14b-instruct-2512" {
				if len(spec.Weights.Sources) != 1 || spec.Weights.Sources[0].Provider != "huggingface" {
					t.Fatalf("sources=%+v, want the downloadable Hugging Face checkpoint only", spec.Weights.Sources)
				}
			}
			args := strings.Join(spec.Args, " ")
			for _, want := range []string{
				"--enforce-eager",
				"--max-model-len 32768",
				"--tokenizer-mode mistral",
				"--config-format mistral",
				"--load-format mistral",
				"--tool-call-parser mistral",
			} {
				if !strings.Contains(args, want) {
					t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
				}
			}
		})
	}
}
