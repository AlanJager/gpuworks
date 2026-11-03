package seed

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

// Plain tensor-parallel text specs no longer pin a card count: their
// resolver accepts any count whose memory holds the weights and the KV
// cache the engine needs at start, so one large card serves a spec that a
// recipe ran on two small ones, and too little memory is still refused.
func TestMemoryRuleSpecsAcceptAnyCardCountThatFits(t *testing.T) {
	cases := []struct {
		model, spec string
		count       int
		memoryGiB   float64
		accept      bool
	}{
		// 29.9 GiB of weights + 16 GiB of KV for a 256K request at 0.85.
		{"qwen/qwen3.8-27b", "int8-vllm-ascend-0a447e8a", 1, 64, true},
		{"qwen/qwen3.8-27b", "int8-vllm-ascend-0a447e8a", 1, 32, false},
		{"qwen/qwen3.8-27b", "int8-vllm-ascend-0a447e8a", 2, 32, true},
		// The published recipe ran four devices; two 64 GiB ones hold it.
		{"google/gemma-4-31b-it", "bf16-vllm-ascend-c7a493cd", 1, 64, false},
		{"google/gemma-4-31b-it", "bf16-vllm-ascend-c7a493cd", 2, 64, true},
	}
	for _, tc := range cases {
		_, spec := catalogSpec(t, tc.model, tc.spec)
		got, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: tc.count, Vendor: "ascend", MinMemoryGiB: tc.memoryGiB,
		})
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.model, tc.spec, err)
		}
		if accepted := got.Reason == ""; accepted != tc.accept {
			t.Errorf("%s/%s on %d×%gGiB: accepted=%v (reason %q), want %v", tc.model, tc.spec, tc.count, tc.memoryGiB, accepted, got.Reason, tc.accept)
		}
		if tc.accept && (got.Variables["TP_SIZE"] != int64(tc.count) || got.Variables["GPU_COUNT"] != int64(tc.count)) {
			t.Errorf("%s/%s: variables %v, want TP_SIZE and GPU_COUNT %d", tc.model, tc.spec, got.Variables, tc.count)
		}
	}
}
