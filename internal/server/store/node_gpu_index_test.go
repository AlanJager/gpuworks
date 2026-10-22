package store

import "testing"

// TestNodeGPUByIndexUsesHardwareIndexNotSlicePosition pins down the one fact
// callers depend on: a GPU index identifies the card, so it must not be read as
// a position in Node.GPUs. A host with cards passed through to other tenants
// reports a sparse, unordered set, where a position lookup silently returns the
// wrong card and runs off the end of the slice.
func TestNodeGPUByIndexUsesHardwareIndexNotSlicePosition(t *testing.T) {
	node := &Node{GPUs: []GPU{
		{Index: 6, UUID: "gpu-6", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4", ComputeCapability: "", MemoryTotal: 32 << 30},
		{Index: 0, UUID: "gpu-0", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotal: 32 << 30},
		{Index: 4, UUID: "gpu-4", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotal: 32 << 30},
		{Index: 3, UUID: "gpu-3", Name: "Ascend910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotal: 32 << 30},
	}}
	for _, index := range []int{0, 3, 4, 6} {
		gpu, ok := node.GPUByIndex(index)
		if !ok {
			t.Fatalf("GPU %d is reported by the node but was not found", index)
		}
		if gpu.Index != index {
			t.Fatalf("GPU %d lookup returned the card at index %d", index, gpu.Index)
		}
		if want := "gpu-" + string(rune('0'+index)); gpu.UUID != want {
			t.Fatalf("GPU %d lookup returned %q, want %q", index, gpu.UUID, want)
		}
	}
	// Indexes the node does not report - including ones that are valid slice
	// positions - must be misses, never a neighbouring card.
	for _, index := range []int{1, 2, 5, 7, -1} {
		if gpu, ok := node.GPUByIndex(index); ok {
			t.Fatalf("GPU %d is not reported by the node but lookup returned %+v", index, gpu)
		}
	}
	if _, ok := (&Node{}).GPUByIndex(0); ok {
		t.Fatal("a node reporting no GPUs returned a card")
	}
	if _, ok := (*Node)(nil).GPUByIndex(0); ok {
		t.Fatal("a nil node returned a card")
	}
}
