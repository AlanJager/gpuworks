package seed

import (
	"strings"
	"testing"
)

func TestMiniMaxM25DoesNotExposeFP8CheckpointAsBF16(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "minimaxai/minimax-m2.5" {
			continue
		}
		nvidiaProfiles := 0
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" {
				continue
			}
			nvidiaProfiles++
			if spec.Weights.Quantization == "bf16" {
				t.Fatalf("%s labels the FP8 checkpoint as BF16", spec.Name)
			}
			if spec.GPUs.CC == "8.0" || spec.GPUs.CC == ">=8.0" {
				t.Fatalf("%s exposes FP8 weights to Ampere", spec.Name)
			}
			if spec.Engine == "sglang" &&
				!strings.Contains(strings.Join(spec.Args, " "), "--quantization") {
				t.Fatalf("%s runs FP8 weights through SGLang without a compatible quantization gate", spec.Name)
			}
		}
		if nvidiaProfiles == 0 {
			t.Fatal("MiniMax-M2.5 has no NVIDIA profile")
		}
		return
	}
	t.Fatal("MiniMax-M2.5 model not found")
}
