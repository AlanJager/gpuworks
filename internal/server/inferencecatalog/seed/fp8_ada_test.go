package seed

import (
	"strings"
	"testing"
)

// FP8 specs opened to Ada (cc >=8.9: L20, L40S, RTX 4090) were run on an L20
// with the vLLM 0.30 runner. Such a spec
// must stay on vLLM and say Ada in its name; a Hopper-only kernel in its
// arguments would not start on Ada.
func TestFP8SpecsOpenedToAdaStayOnVerifiedShapes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" || spec.GPUs.CC != ">=8.9" || spec.Weights.Quantization != "fp8" {
				continue
			}
			opened++
			where := model.ModelID + "/" + spec.Name
			if spec.Engine != "vllm" {
				t.Errorf("%s opens FP8 to Ada on %s, which was not run on Ada with the platform image", where, spec.Engine)
			}
			if !strings.HasPrefix(spec.DisplayNameZHCN, "Ada 及以上") || !strings.HasPrefix(spec.DisplayNameEN, "Ada+") {
				t.Errorf("%s is named %q / %q", where, spec.DisplayNameZHCN, spec.DisplayNameEN)
			}
			for _, hopperOnly := range []string{"deep_gemm", "fa3", "flashmla", "cutlass_mla", "trtllm"} {
				if strings.Contains(strings.Join(spec.Args, " "), hopperOnly) {
					t.Errorf("%s passes the Hopper-only %s", where, hopperOnly)
				}
			}
		}
	}
	if opened < 40 {
		t.Fatalf("only %d FP8 specs match Ada", opened)
	}
}
