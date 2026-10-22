package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeWithForeignCards is a node of four cards where only two are its own:
// the layout zstone-164 has, in miniature.
func nodeWithForeignCards() *store.Node {
	return &store.Node{
		ID: 1, UUID: "node-1", State: store.NodeStateOnline, GPUCount: 2,
		GPUs: []store.GPU{
			{Index: 0, Ident: "pci:0000:3f:00.0", UUID: "GPU-3f", Name: "A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 80 << 30},
			{Index: 1, Ident: "pci:0000:40:00.0", UUID: "GPU-40", Name: "A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 80 << 30},
			// Given to a virtual machine: present, not ours, not a fault.
			{Index: -1, Ident: "pci:0000:1b:00.0", Name: "A100", Vendor: "nvidia", MemoryTotal: 0, Ownership: accelerator.CardPassedThrough},
			// No driver bound at all.
			{Index: -1, Ident: "pci:0000:1a:00.0", Name: "A100", Vendor: "nvidia", Ownership: accelerator.CardWithoutDriver},
		},
	}
}

// Capacity is only ever the node's own cards. A card handed to a virtual
// machine is hardware an operator can see, and nobody's capacity: offering
// it would place a replica on a card this host cannot touch.
func TestCapacityCountsOnlyTheNodesOwnCards(t *testing.T) {
	node := nodeWithForeignCards()
	if got := len(node.OwnedGPUs()); got != 2 {
		t.Fatalf("owned cards = %d, want 2", got)
	}
	if got := FreeHealthyGPUs(node, nil); got != 2 {
		t.Fatalf("free healthy cards = %d, want 2; the cards owned elsewhere are not free", got)
	}
	if got := FreeHealthyGPUs(node, map[int]bool{0: true}); got != 1 {
		t.Fatalf("free with one held = %d, want 1", got)
	}

	// A card with no index cannot be looked up by one, and must not be
	// found by the -1 the passed-through cards share.
	if _, ok := node.GPUByIndex(-1); ok {
		t.Fatal("a card with no device index was found by index -1")
	}
	if card, ok := node.GPUByIndex(0); !ok || card.UUID != "GPU-3f" {
		t.Fatalf("lookup of device 0 = %+v, %v", card, ok)
	}
}

// The scheduler never sees the cards this node does not own, so no plan can
// name one.
func TestNodeSnapshotHidesCardsOwnedElsewhere(t *testing.T) {
	st, err := store.Open("file:planning_card_ownership?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	snapshot := NodeSnapshot(st, nodeWithForeignCards(), true, "")
	if len(snapshot.GPUs) != 2 {
		t.Fatalf("snapshot cards = %+v, want only the node's own two", snapshot.GPUs)
	}
	for _, gpu := range snapshot.GPUs {
		if gpu.Index < 0 {
			t.Fatalf("a card with no device index reached the scheduler: %+v", gpu)
		}
		if gpu.MemoryTotal == 0 {
			t.Fatalf("a card with no memory reached the scheduler: %+v", gpu)
		}
	}
}

// What the node permanently is, for pooling, counts its own cards only;
// otherwise two nodes differing just in how many cards are lent to VMs
// would read as different hardware and be judged separately.
func TestHardwareShapeIgnoresCardsOwnedElsewhere(t *testing.T) {
	withForeign := HardwareShape(nodeWithForeignCards(), true)
	ownOnly := nodeWithForeignCards()
	ownOnly.GPUs = ownOnly.OwnedGPUs()
	if got := HardwareShape(ownOnly, true); got != withForeign {
		t.Fatalf("shape with foreign cards = %q, without = %q; they must match", withForeign, got)
	}
}
