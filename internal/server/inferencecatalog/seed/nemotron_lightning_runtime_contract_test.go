package seed

import (
	"strings"
	"testing"
)

func TestNemotronLightningDoesNotRequireMissingFastokens(t *testing.T) {
	_, spec := catalogSpec(t, "nvidia/nvidia-nemotron-3.5-lightning-30b-a3b", "bf16-vllm-nvidia-general")
	if value := spec.Env["VLLM_USE_FASTOKENS"]; value != "" {
		t.Errorf("A100 profile requests an unavailable accelerator: VLLM_USE_FASTOKENS=%s", value)
	}
	args := strings.Join(spec.Args, " ")
	if !strings.Contains(args, "--moe-backend auto") {
		t.Errorf("A100 profile must let vLLM select an Ampere-compatible MoE backend: %v", spec.Args)
	}
}
