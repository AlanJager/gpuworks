package seed

import (
	"slices"
	"testing"
)

func TestHunyuanA13BUsesBuiltinSGLangModelCode(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "tencent/hunyuan-a13b-instruct" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Engine != "sglang" || spec.GPUs.Vendor != "nvidia" {
				continue
			}
			if spec.Image != "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2" {
				t.Fatalf("%s image=%q, want SGLang 0.5.21 runner", spec.Name, spec.Image)
			}
			if spec.GPUs.CC != ">=8.0" {
				t.Fatalf("%s cc=%q, want Ampere+", spec.Name, spec.GPUs.CC)
			}
			if slices.Contains(spec.Args, "--trust-remote-code") {
				t.Fatalf("%s args=%v, want builtin Hunyuan model code", spec.Name, spec.Args)
			}
			return
		}
		t.Fatal("Hunyuan-A13B has no NVIDIA SGLang profile")
	}
	t.Fatal("Hunyuan-A13B model not found")
}
