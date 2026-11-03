package seed

import (
	"strings"
	"testing"
)

func TestMistralSmall4HasVerifiedA100Profile(t *testing.T) {
	_, spec := catalogSpec(
		t,
		"mistralai/mistral-small-4-119b-2603",
		"fp8-vllm-nvidia-a100",
	)
	if spec.Engine != "vllm" || spec.Image != "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2" {
		t.Fatalf("runtime=%s image=%q, want verified vLLM A100 runner", spec.Engine, spec.Image)
	}
	if spec.GPUs.Vendor != "nvidia" || spec.GPUs.CC != "8.0" {
		t.Fatalf("gpus=%+v, want A100 CC 8.0", spec.GPUs)
	}
	if spec.Weights.Quantization != "fp8" {
		t.Fatalf("quantization=%q, want fp8", spec.Weights.Quantization)
	}
	if spec.RuntimeWeightBytes != 120922770560 {
		t.Fatalf("runtime_weight_bytes=%v, want measured FP8 runtime weight footprint", spec.RuntimeWeightBytes)
	}
	args := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"--trust-remote-code",
		"--enforce-eager",
		"--max-model-len 32768",
		"--enable-auto-tool-choice",
		"--tool-call-parser mistral",
		"--reasoning-parser mistral",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
		}
	}
}
