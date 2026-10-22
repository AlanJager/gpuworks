package seed

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

// Strata uses the cards the platform assigns: its resolver accepts any card
// count whose every card has the 12 GiB the engine needs, and pins no count
// or degree of its own, so a deployment that selects two cards gets two
// (the entrypoint passes them on as --gpus).
func TestStrataProfileRunsOnTheAssignedCards(t *testing.T) {
	_, spec := ppuSpec(t, "ista-daslab/qwen3.8-flash-next-strata", "iq2xs-strata-nvidia-single-gpu")
	resolve := func(count int, memoryGiB float64) placement.Result {
		t.Helper()
		names := make([]string, count)
		memories := make([]float64, count)
		for index := range names {
			names[index], memories[index] = "NVIDIA GeForce RTX 4090 D", memoryGiB
		}
		result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: count, Vendor: "nvidia", Names: names, MemoriesGiB: memories, MinMemoryGiB: memoryGiB,
		})
		if err != nil {
			t.Fatalf("count=%d memory=%v: %v", count, memoryGiB, err)
		}
		return result
	}
	for _, count := range []int{1, 2, 4, 8} {
		result := resolve(count, 24)
		if result.Reason != "" {
			t.Fatalf("%d cards of 24 GiB rejected: %s", count, result.Reason)
		}
		for _, pinned := range []string{"GPU_COUNT", "TP_SIZE"} {
			if value, ok := result.Variables[pinned]; ok {
				t.Errorf("%d cards: the resolver pins %s=%v instead of using the assigned cards", count, pinned, value)
			}
		}
	}
	if result := resolve(2, 11); result.Reason == "" {
		t.Error("cards below 12 GiB are accepted")
	}
	// The template, not the platform's weight-based estimate, states what a
	// card must hold: Strata tiers its 63 GiB of experts across VRAM, RAM and
	// SSD, so admission is the 12 GiB the engine declares on each card. The
	// contract lives in the resolver, so it travels with every copy of the
	// template, and the spec carries no runtime_weight_bytes stand-in for it.
	if result := resolve(1, 48); result.GPUMemoryMiBPerDevice != 12288 {
		t.Errorf("per-card admission contract = %d MiB, want 12288", result.GPUMemoryMiBPerDevice)
	}
	if spec.RuntimeWeightBytes != 0 {
		t.Errorf("runtime_weight_bytes = %d; the admission line belongs to the resolver", spec.RuntimeWeightBytes)
	}
}
