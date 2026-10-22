package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// How much of a status report's card list may be written, and what a
// failed probe is allowed to say. A PCI read is authoritative about which
// cards are in the machine and who holds each one; what a card IS — its
// memory, its identity, its health — only the vendor CLI can answer, and
// a probe that could not ask must not overwrite it.

// A probe that failed knows which cards are in the machine (sysfs says so)
// but nothing about them. That is worth storing for a node that has never
// had a successful probe — a host whose driver cannot bring its cards up
// would otherwise read as having no accelerators at all — and must never
// replace what a successful probe stored, since an instance is placed on a
// card's memory and identity.
func TestAFailedProbeNeverOverwritesWhatOnlyTheCLIKnows(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-weak", HashToken("t"), "weak-host"); err != nil {
		t.Fatal(err)
	}
	failed := false
	// Nothing known yet: take what sysfs could tell us.
	fromSysfs := []proto.GPUInfo{
		{Index: -1, Vendor: "nvidia", Name: "GA102 [GeForce RTX 3090]", PCIeBDF: "0000:1c:00.0",
			HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthDriverInitFailed},
		{Index: -1, Vendor: "nvidia", Name: "GA102 [GeForce RTX 3090]", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
	}
	if err := s.UpdateNodeStatus("uuid-weak", proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "nvidia-smi: no answer within 10s", GPUs: fromSysfs}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-weak")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 2 {
		t.Fatalf("cards = %+v, want the two sysfs found; a host whose driver is wedged still has hardware", node.GPUs)
	}
	if node.GPUCount != 1 {
		t.Fatalf("gpu_count = %d, want 1: only the card this node owns, and it cannot serve", node.GPUCount)
	}

	// A successful probe fills in what the driver knows.
	ok := true
	full := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-1c", Vendor: "nvidia", Name: "NVIDIA GeForce RTX 3090", PCIeBDF: "0000:1c:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
		{Index: -1, Vendor: "nvidia", Name: "GA102 [GeForce RTX 3090]", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
	}
	if err := s.UpdateNodeStatus("uuid-weak", proto.NodeStatus{GPUProbeOK: &ok, GPUs: full}); err != nil {
		t.Fatal(err)
	}

	// Now a probe fails again, reporting only what sysfs says. The card
	// must keep its memory and UUID: replicas are placed on them.
	if err := s.UpdateNodeStatus("uuid-weak", proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "nvidia-smi: no answer within 10s", GPUs: fromSysfs}); err != nil {
		t.Fatal(err)
	}
	node, err = s.NodeWithGPUs("uuid-weak")
	if err != nil {
		t.Fatal(err)
	}
	var card GPU
	for _, c := range node.GPUs {
		if c.Ident == "pci:0000:1c:00.0" {
			card = c
		}
	}
	if card.UUID != "GPU-1c" || card.MemoryTotal != 24<<30 || !card.Healthy() {
		t.Fatalf("the card lost what the driver knew: %+v", card)
	}
}

// A card's derived columns are filled by GPU.BeforeSave, which gorm runs
// for Create and Save but not for a single-column Update. A writer that
// changed a card's health with one would leave the verdict the fleet
// queries read behind — correct data, and a working card nothing would
// schedule. Nothing in the server does that; this says so, and says why if
// something starts to.
// A model no PCI rule recognizes, that the vendor CLI nevertheless
// reports, is stored and schedulable like any other card. The PCI table
// can only add cards the CLI could not describe; it is never a gate on
// the ones it could.
//
// internal/agent/gpu pins the agent's half of this. This is the other
// end: what the server does with such a card once it arrives, which is
// what "usable on the platform" actually means.
func TestACardOnlyTheVendorToolKnowsIsStoredAndSchedulable(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-new-model", HashToken("t"), "new-model-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	// No Ownership and no health finding: all the vendor CLI said is that
	// the card is there and well. Its PCI ids are not in any table.
	report := proto.GPUInfo{
		Index: 0, UUID: "GPU-unheard-of", Name: "Some Accelerator X1", Vendor: "nvidia",
		PCIeBDF: "0000:5e:00.0", MemoryTotalBytes: 141 << 30, Healthy: true,
	}
	if err := s.UpdateNodeStatus("uuid-new-model", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{report}}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-new-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 1 {
		t.Fatalf("stored %d cards, want the one the tool reported: %+v", len(node.GPUs), node.GPUs)
	}
	card := node.GPUs[0]
	if !card.CanTakeWork() {
		t.Fatalf("card = %+v, want it schedulable", card)
	}
	if card.Ownership != accelerator.CardOwnedByNode || card.HealthStatus != accelerator.HealthOK {
		t.Fatalf("card = %+v, want this node's own and healthy", card)
	}
	if card.MemoryTotal != 141<<30 || card.UUID != "GPU-unheard-of" {
		t.Fatalf("the tool's facts were not kept: %+v", card)
	}
	if len(node.OwnedGPUs()) != 1 || node.GPUCount != 1 {
		t.Fatalf("owned=%d count=%d, want the card to count towards capacity", len(node.OwnedGPUs()), node.GPUCount)
	}
	// And the fleet query the scheduler reads agrees with the row.
	if err := s.SetNodeState("uuid-new-model", NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	groups, err := s.OnlineGPUGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Cards != 1 || groups[0].Unusable != 0 {
		t.Fatalf("fleet groups = %+v, want one usable card", groups)
	}
}

// A host whose vendor CLI never answers still has its cards tracked. The
// rows used to be written once and frozen: 172.30.8.31 reported every
// minute for half an hour with its card rows untouched, so a card later
// handed to a virtual machine would never have been noticed, and the node
// would have gone on offering capacity it no longer had.
func TestAFailedProbeStillTracksWhatPCIReads(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-wedged", HashToken("t"), "wedged"); err != nil {
		t.Fatal(err)
	}
	ok, failed := true, false
	card := func(bdf, ownership string, index int) proto.GPUInfo {
		g := proto.GPUInfo{Index: index, Name: "910B4", Vendor: "ascend", PCIeBDF: bdf, Ownership: ownership}
		if ownership == accelerator.CardOwnedByNode {
			g.UUID, g.MemoryTotalBytes, g.Healthy = "NPU-"+bdf[5:7], 32<<30, true
		}
		return g
	}
	// A good probe: two cards, both this node's, with the CLI's facts.
	if err := s.UpdateNodeStatus("uuid-wedged", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{
		card("0000:41:00.0", accelerator.CardOwnedByNode, 0),
		card("0000:c1:00.0", accelerator.CardOwnedByNode, 1),
	}}); err != nil {
		t.Fatal(err)
	}
	// The CLI stops answering, and one card is handed to a virtual
	// machine. All the agent can read is the PCI bus.
	if err := s.UpdateNodeStatus("uuid-wedged", proto.NodeStatus{
		GPUProbeOK: &failed, GPUProbeError: "npu-smi: dcmi module initialize failed",
		GPUs: []proto.GPUInfo{
			card("0000:41:00.0", accelerator.CardOwnedByNode, -1),
			card("0000:c1:00.0", accelerator.CardPassedThrough, -1),
		},
	}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-wedged")
	if err != nil {
		t.Fatal(err)
	}
	byBDF := map[string]GPU{}
	for _, g := range node.GPUs {
		byBDF[g.PCIeBDF] = g
	}
	lent := byBDF["0000:c1:00.0"]
	if lent.Ownership != accelerator.CardPassedThrough {
		t.Fatalf("the card given to a VM = %+v, want the new ownership PCI read", lent)
	}
	if lent.CanTakeWork() || lent.HealthStatus != accelerator.HealthOK {
		t.Fatalf("a card lent elsewhere = %+v, want it off the scheduler and not called faulty", lent)
	}
	if node.GPUCount != 1 {
		t.Fatalf("capacity = %d, want the one card still this node's", node.GPUCount)
	}
	// The card still this node's keeps what only the CLI could say. The
	// reading is stale, which the node's probe error states.
	kept := byBDF["0000:41:00.0"]
	if kept.UUID != "NPU-41" || kept.MemoryTotal != 32<<30 || !kept.CanTakeWork() {
		t.Fatalf("the remaining card = %+v, want the CLI's last facts kept", kept)
	}
	if node.GPUProbeError == "" {
		t.Fatal("the node does not say its readings are stale")
	}
}

// A failed probe that read no PCI bus at all — a container that does not
// map sysfs — says nothing, and nothing changes.
func TestAFailedProbeWithNoRosterChangesNothing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-blind", HashToken("t"), "blind"); err != nil {
		t.Fatal(err)
	}
	ok, failed := true, false
	good := proto.GPUInfo{Index: 0, UUID: "GPU-a", Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 80 << 30, Healthy: true}
	if err := s.UpdateNodeStatus("uuid-blind", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{good}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-blind", proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "nvidia-smi: no answer"}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-blind")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].UUID != "GPU-a" || node.GPUCount != 1 {
		t.Fatalf("cards = %+v count = %d, want the last good probe's untouched", node.GPUs, node.GPUCount)
	}
}

// A card the vendor CLI has never described has no CLI facts to protect,
// so a later PCI read may rename it. Without this the first name a card
// ever got is frozen: 172.30.8.31's cards stayed "19e5:d802" after the
// agent learned to call them 910B4, because the rule meant to protect
// the CLI's facts was protecting a PCI fallback.
func TestAPCIOnlyCardTakesABetterNameLater(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-renamed", HashToken("t"), "renamed"); err != nil {
		t.Fatal(err)
	}
	failed := false
	report := func(name string) proto.NodeStatus {
		return proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "npu-smi: no answer", GPUs: []proto.GPUInfo{
			{Index: -1, Vendor: "ascend", Name: name, PCIeBDF: "0000:41:00.0",
				HealthCode: accelerator.HealthCLIUnanswered, HealthStatus: accelerator.HealthError},
		}}
	}
	// An agent with no model names of its own: all it can say is the ids.
	if err := s.UpdateNodeStatus("uuid-renamed", report("19e5:d802")); err != nil {
		t.Fatal(err)
	}
	// The agent is upgraded and now carries the name.
	if err := s.UpdateNodeStatus("uuid-renamed", report("910B4")); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-renamed")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 1 || node.GPUs[0].Name != "910B4" {
		t.Fatalf("card = %+v, want the better name the newer agent reads", node.GPUs)
	}
}

// A probe that could only read PCI may change only what PCI is
// authoritative for. A second column list once said which those were, and
// "Index" slipped into it: a device index is what the vendor CLI hands
// out, so one routine timeout wrote -1 over every card and the planner
// stopped finding the cards its instances were bound to. The row is now
// built from the stored one, and this pins what that row keeps.
func TestAPresenceOnlyRowKeepsWhatOnlyTheCLIKnows(t *testing.T) {
	stored := GPU{
		ID: 9, NodeID: 7, Ident: "pci:0000:3f:00.0", Index: 3, UUID: "GPU-a", Name: "NVIDIA A100-SXM4-80GB",
		ComputeCapability: "8.0", Vendor: "nvidia", ArchID: "ga100", MemoryTotal: 80 << 30, PowerLimitWatts: 400,
		PCIeBDF: "0000:3f:00.0", NUMANode: 0, MIGEnabled: true, ForeignProcesses: `[{"pid":1,"name":"vllm"}]`,
		HealthStatus: accelerator.HealthWarning, HealthCode: accelerator.HealthMemoryRepairPending, HealthMessage: "x: y",
	}
	// What a probe that could not ask the CLI carries: every CLI field
	// empty or -1, and a PCI read that moved the card to another NUMA node.
	fromPCI := newCardRow(7, stored.Ident, proto.GPUInfo{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", NUMANode: 1})
	row := presenceOnlyRow(stored, fromPCI)

	// Only the vendor's own tool can answer these; a PCI read cannot.
	if row.Index != stored.Index || row.UUID != stored.UUID || row.MemoryTotal != stored.MemoryTotal ||
		row.ComputeCapability != stored.ComputeCapability || row.ArchID != stored.ArchID ||
		row.PowerLimitWatts != stored.PowerLimitWatts || row.MIGEnabled != stored.MIGEnabled ||
		row.ForeignProcesses != stored.ForeignProcesses || row.Name != stored.Name {
		t.Fatalf("a failed probe overwrote what only the CLI knows:\nstored %+v\nwrote  %+v", stored, row)
	}
	// A card still this node's keeps its last finding: stale, which the
	// node's probe error says, but better than none.
	if row.HealthStatus != stored.HealthStatus || row.HealthCode != stored.HealthCode {
		t.Fatalf("health = %s/%s, want the last finding kept", row.HealthStatus, row.HealthCode)
	}
	// What PCI is authoritative for is taken from it.
	if row.NUMANode != 1 {
		t.Fatalf("numa = %d, want the PCI read", row.NUMANode)
	}
}

// A card handed back by a virtual machine while the vendor CLI is still
// down used to keep the "cannot serve" the passed-through row carried,
// with no finding code and no evidence: an error an operator cannot act
// on. It now carries what this probe does know — that it could not ask.
func TestACardComingBackFromAVMSaysWhatIsKnownOfIt(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("returned", HashToken("t"), "returned"); err != nil {
		t.Fatal(err)
	}
	failed := false
	report := func(ownership string) proto.NodeStatus {
		g := proto.GPUInfo{Index: -1, Vendor: "ascend", Name: "910B4", PCIeBDF: "0000:41:00.0", Ownership: ownership}
		if ownership == accelerator.CardOwnedByNode {
			g.HealthCode, g.HealthStatus = accelerator.HealthCLIUnanswered, accelerator.HealthError
		}
		return proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "npu-smi: no answer", GPUs: []proto.GPUInfo{g}}
	}
	if err := s.UpdateNodeStatus("returned", report(accelerator.CardPassedThrough)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("returned", report(accelerator.CardOwnedByNode)); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("returned")
	if err != nil {
		t.Fatal(err)
	}
	card := node.GPUs[0]
	if card.HealthCode != accelerator.HealthCLIUnanswered {
		t.Fatalf("card = %+v, want it to say the CLI could not be asked rather than a bare Error", card)
	}
	if card.CanTakeWork() {
		t.Fatal("a card nothing is known about was offered to the scheduler")
	}
}

// One card reporting a key another already used must not fail the node's
// status transaction: the cards are written inside it, so a unique-index
// error would roll back the node's state and last-seen as well.
func TestADuplicateCardKeyDoesNotFailTheNodesReport(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("dup", HashToken("t"), "dup"); err != nil {
		t.Fatal(err)
	}
	ok := true
	same := proto.GPUInfo{Index: 0, UUID: "GPU-a", Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true}
	if err := s.UpdateNodeStatus("dup", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{same, same}}); err != nil {
		t.Fatalf("a duplicated card key failed the whole report: %v", err)
	}
	node, err := s.NodeWithGPUs("dup")
	if err != nil {
		t.Fatal(err)
	}
	if len(node.GPUs) != 1 {
		t.Fatalf("stored %d rows for one card", len(node.GPUs))
	}
	if node.LastSeenAt == nil {
		t.Fatal("the node's own status was rolled back with the card")
	}
}
