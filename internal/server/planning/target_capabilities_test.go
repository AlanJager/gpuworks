package planning

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"testing"
)

func TestNeedsTargetPreflightUsesOwnedPhysicalCards(t *testing.T) {
	cases := []struct {
		name string
		node *store.Node
		want bool
	}{
		{name: "nil node"},
		{name: "nvidia", node: &store.Node{GPUVendor: "nvidia", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}, want: true},
		{name: "huawei", node: &store.Node{GPUVendor: "ascend", GPUs: []store.GPU{{Index: 0, Vendor: "ascend", Name: "910B"}}}, want: true},
		{name: "apple", node: &store.Node{GPUVendor: "apple", GPUs: []store.GPU{{Index: 0, Vendor: "apple", Name: "Apple GPU"}}}, want: true},
		{name: "AMD owned", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X"}}}, want: true},
		{name: "mixed includes AMD", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}, {Index: 1, Vendor: "amd", Name: "MI300X"}}}, want: true},
		{name: "AMD passed through", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X", Ownership: "passthrough"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsTargetPreflight(tc.node); got != tc.want {
				t.Fatalf("NeedsTargetPreflight()=%t want %t", got, tc.want)
			}
		})
	}
}

func TestResolvedFeaturesAreAppliedToEveryVendor(t *testing.T) {
	in := Input{
		Engine:            "vllm",
		RequestedFeatures: &proto.RequestedFeatures{EngineSleep: true},
	}
	features := requestedInputFeaturesForVendor(in, accelerator.VendorAMD)
	found := false
	for _, feature := range features {
		found = found || feature == accelerator.FeatureEngineSleep
	}
	if !found {
		t.Fatalf("AMD did not infer the requested launch feature: %v", features)
	}
	apple := &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "apple", Name: "Apple GPU"}}}
	appleInput := Input{Node: apple, Engine: "vllm", Sleepable: true, RequestedFeatures: in.RequestedFeatures}
	snapshot := scheduler.NodeSnapshot{GPUs: []scheduler.GPU{{Index: 0}}}
	if refusal := constrainTargetGPUs(appleInput, &snapshot); refusal == nil || refusal.Code != scheduler.ReasonSleepUnsupportedOnNode {
		t.Fatalf("Apple's declared sleep restriction was not preserved: %+v", refusal)
	}
}
