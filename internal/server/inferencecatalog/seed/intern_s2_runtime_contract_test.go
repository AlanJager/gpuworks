package seed

import (
	"strings"
	"testing"
)

func TestInternS2SGLangExecutesItsCheckpointCode(t *testing.T) {
	_, spec := catalogSpec(t, "internlm/intern-s2-preview", "bf16-sglang-nvidia-general")
	args := strings.Join(spec.Args, " ")
	if !strings.Contains(args, "--trust-remote-code") {
		t.Errorf("SGLang launch arguments do not load the checkpoint code: %v", spec.Args)
	}
}
