package seed

import (
	"strings"
	"testing"
)

func TestSeedOSSBoundsItsKVFootprintOnA100(t *testing.T) {
	_, spec := catalogSpec(t, "bytedance-seed/seed-oss-36b-instruct", "bf16-vllm-nvidia-general")
	args := strings.Join(spec.Args, " ")
	if !strings.Contains(args, "--max-model-len 131072") {
		t.Errorf("A100 launch arguments do not bound the context: %v", spec.Args)
	}
}
