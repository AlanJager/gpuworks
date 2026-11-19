package seed

import (
	"slices"
	"strings"
	"testing"
)

// GWKS-354: on 910B4 the Gemma 3 27B SGLang profile started in NPU graph mode
// and its schedulers crashed at the first generation; the eager path came up
// and answered chat. The 910B profile therefore disables graph capture, as
// its 910C counterpart already did, and says why.
func TestGemma3_27BOn910BRunsEager(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const flag = "--disable-cuda-graph"
	found := 0
	for _, model := range catalog.Models {
		if model.ModelID != "google/gemma-3-27b-it" {
			continue
		}
		for _, spec := range model.Specs {
			if !strings.HasPrefix(spec.Name, "bf16-sglang-ascend-") {
				continue
			}
			found++
			if !slices.Contains(spec.Args, flag) {
				t.Errorf("%s does not disable graph capture: %v", spec.Name, spec.Args)
			}
			explained := false
			for _, tradeoff := range spec.PerformanceTradeoffs {
				explained = explained || tradeoff.Param == flag
			}
			if !explained {
				t.Errorf("%s gives up graph mode without saying why", spec.Name)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d Ascend SGLang profiles of gemma-3-27b-it, want the 910B and the 910C one", found)
	}
}
