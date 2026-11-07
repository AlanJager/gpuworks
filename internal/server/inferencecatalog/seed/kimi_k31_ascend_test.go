package seed

import (
	"strings"
	"testing"
)

func TestKimiK31AscendColocatedLaunchContract(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.Name != "w4a8-vllm-ascend-a3-multinode-16npu" {
				continue
			}
			args := strings.Join(spec.Args, " ")
			if strings.Contains(args, "recompute_scheduler_enable") {
				t.Fatalf("co-located launch must not enable Decode-only recompute: %s", args)
			}
			for _, required := range []string{"--max-model-len 210000", "--max-num-seqs 64", "--gpu-memory-utilization 0.90"} {
				if !strings.Contains(args, required) {
					t.Errorf("launch args missing %q: %s", required, args)
				}
			}
			return
		}
	}
	t.Fatal("Kimi K3.1 Pro Ascend co-located spec not found")
}
