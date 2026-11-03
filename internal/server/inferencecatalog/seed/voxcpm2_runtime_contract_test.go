package seed

import (
	"slices"
	"testing"
)

func TestVoxCPM2UsesRunnerWithVoxCPM(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "openbmb/voxcpm2" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" {
				continue
			}
			if spec.Image != "gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.30.0-r4" {
				t.Fatalf("%s image=%q, want VoxCPM-enabled runner r4", spec.Name, spec.Image)
			}
			if !slices.Contains(spec.Args, "--omni") {
				t.Fatalf("%s args=%v, want --omni", spec.Name, spec.Args)
			}
			return
		}
		t.Fatal("VoxCPM2 has no NVIDIA profile")
	}
	t.Fatal("VoxCPM2 model not found")
}
