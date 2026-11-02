package seed

import (
	"slices"
	"testing"
)

func TestGLM45VA3UsesMeasuredEagerPath(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, model := range catalog.Models {
		if model.ModelID != "zai-org/glm-4.5v" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name != "bf16-sglang-ascend-a3-36013872" {
				continue
			}
			found = true
			if !slices.Contains(spec.Args, "--disable-cuda-graph") {
				t.Errorf("A3 profile must disable graph capture: %q", spec.Args)
			}
		}
	}
	if !found {
		t.Fatal("GLM-4.5V A3 profile is missing")
	}
}
