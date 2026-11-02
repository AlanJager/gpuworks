package seed

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

func TestMeasuredSingleDieA3ProfilesRejectMultiDevicePlacement(t *testing.T) {
	profiles := []struct {
		model string
		spec  string
	}{
		{"qwen/qwen2.5-1.5b", "bf16-sglang-ascend-a3-qwen25-1p5b"},
		{"qwen/qwen3-1.7b", "bf16-sglang-ascend-a3-qwen3-1p7b"},
		{"qwen/qwen3-4b", "bf16-sglang-ascend-a3-qwen3-4b"},
		{"qwen/qwen2.5-coder-0.5b-instruct", "bf16-sglang-ascend-a3-qwen25-coder-05b"},
		{"qwen/qwen2.5-coder-1.5b-instruct", "bf16-sglang-ascend-a3-qwen25-coder-15b"},
		{"huggingfacetb/smollm2-360m-instruct", "bf16-sglang-ascend-a3-smollm2-360m"},
		{"qwen/qwen3-tts-12hz-0.6b-customvoice", "bf16-vllm-ascend-a3-tts06"},
		{"paddlepaddle/paddleocr-vl-1.6", "bf16-vllm-ascend-a3-paddleocr16"},
	}
	for _, profile := range profiles {
		t.Run(profile.model, func(t *testing.T) {
			_, spec := ppuSpec(t, profile.model, profile.spec)
			accepted, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
				Count: 1, Vendor: "ascend", MinMemoryGiB: 32,
			})
			if err != nil || accepted.Reason != "" {
				t.Fatalf("one-device placement rejected: %+v %v", accepted, err)
			}
			rejected, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
				Count: 2, Vendor: "ascend", MinMemoryGiB: 32,
			})
			if err != nil || rejected.Reason == "" {
				t.Fatalf("multi-device placement was not rejected: %+v %v", rejected, err)
			}
		})
	}
}

func TestGemma3VLLMTPDivisibility(t *testing.T) {
	_, spec := ppuSpec(t, "google/gemma-3-4b-it", "bf16-vllm-ascend-a3-gemma3-4b")
	for _, count := range []int{1, 2, 4, 8} {
		accepted, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: count, Vendor: "ascend", MinMemoryGiB: 32,
		})
		if err != nil || accepted.Reason != "" {
			t.Errorf("TP%d placement rejected: %+v %v", count, accepted, err)
		}
	}
	for _, count := range []int{3, 16} {
		rejected, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: count, Vendor: "ascend", MinMemoryGiB: 32,
		})
		if err != nil || rejected.Reason == "" {
			t.Errorf("TP%d placement was not rejected: %+v %v", count, rejected, err)
		}
	}
}

func TestQwen3NextEagerWorkaroundStaysA3Only(t *testing.T) {
	_, original := ppuSpec(t, "qwen/qwen3-next-80b-a3b-instruct", "int8-sglang-ascend-7d129784")
	_, a3 := ppuSpec(t, "qwen/qwen3-next-80b-a3b-instruct", "int8-sglang-ascend-a3-7d129784")
	if slices.Contains(original.Args, "--disable-cuda-graph") {
		t.Error("910B original contains the A3-only graph workaround")
	}
	if !slices.Contains(a3.Args, "--disable-cuda-graph") {
		t.Error("A3 profile lost the measured graph workaround")
	}
}

func TestGemma431UsesCheckpointLocalChatTemplate(t *testing.T) {
	for _, specName := range []string{"bf16-vllm-ascend-c7a493cd", "bf16-vllm-ascend-a3-c7a493cd"} {
		_, spec := ppuSpec(t, "google/gemma-4-31b-it", specName)
		if !containsArgumentPair(spec.Args, "--chat-template", "/model/chat_template.jinja") {
			t.Errorf("%s does not use the checkpoint-local chat template", specName)
		}
	}
}
