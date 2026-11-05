package store

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The vendors on the host's bus are replaced whole by every report, and a
// report that lists none — an older agent, a container — leaves nothing.
// Switching the managed vendor replaces the cards: the other vendor's are
// gone after the first report that no longer lists them.
func TestTheVendorsOnTheBusFollowEachReport(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-mixed", HashToken("t"), "mixed-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	both := []proto.AcceleratorPresence{{Vendor: "ascend", Cards: 1, Model: "310P3"}, {Vendor: "nvidia", Cards: 1, Model: "AD102 [GeForce RTX 4090]"}}
	ascend := []proto.GPUInfo{{Index: 4, UUID: "ascend-4", Name: "310P3", Vendor: "ascend", PCIeBDF: "0000:17:00.0", MemoryTotalBytes: 21 << 30, Healthy: true}}
	nvidia := []proto.GPUInfo{{Index: 0, UUID: "GPU-5a05", Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", PCIeBDF: "0000:65:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	report := func(vendor string, cards []proto.GPUInfo, vendors []proto.AcceleratorPresence) *Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-mixed", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{GPUVendor: vendor, AcceleratorVendors: vendors}}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeWithGPUs("uuid-mixed")
		if err != nil {
			t.Fatal(err)
		}
		return node
	}

	node := report("ascend", ascend, both)
	if got := node.AcceleratorVendorList(); !reflect.DeepEqual(got, both) {
		t.Fatalf("vendors = %#v", got)
	}
	if !node.HostHasAcceleratorVendor("nvidia") || !node.HostHasAcceleratorVendor("ascend") || node.HostHasAcceleratorVendor("hygon") {
		t.Fatalf("HostHasAcceleratorVendor wrong for %s", node.AcceleratorVendors)
	}
	if node.GPUVendor != "ascend" || len(node.GPUs) != 1 || node.GPUs[0].Vendor != "ascend" {
		t.Fatalf("before the switch: vendor %q cards %+v", node.GPUVendor, node.GPUs)
	}

	node = report("nvidia", nvidia, both)
	if node.GPUVendor != "nvidia" || len(node.GPUs) != 1 || node.GPUs[0].Vendor != "nvidia" || node.GPUs[0].UUID != "GPU-5a05" {
		t.Fatalf("after the switch: vendor %q cards %+v", node.GPUVendor, node.GPUs)
	}

	node = report("nvidia", nvidia, nil)
	if node.AcceleratorVendors != "" || node.AcceleratorVendorList() != nil || node.HostHasAcceleratorVendor("nvidia") {
		t.Fatalf("a report without vendors left %q", node.AcceleratorVendors)
	}
}

func TestAnUnreadableVendorListIsEmpty(t *testing.T) {
	for _, raw := range []string{"", " ", "{", `{"vendor":"nvidia"}`} {
		node := &Node{AcceleratorVendors: raw}
		if got := node.AcceleratorVendorList(); got != nil {
			t.Fatalf("%q decoded to %#v", raw, got)
		}
	}
	var none *Node
	if none.AcceleratorVendorList() != nil {
		t.Fatal("a nil node listed vendors")
	}
	if (&Node{AcceleratorVendors: `[{"vendor":"nvidia","cards":0}]`}).HostHasAcceleratorVendor("nvidia") {
		t.Fatal("a vendor with no cards counted")
	}
}

// The choice is stored in its canonical spelling and replayed to the agent
// in the desired state; an unknown one is refused before it is stored.
func TestTheAcceleratorChoiceIsStoredAndReplayed(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("uuid-choice", HashToken("t"), "choice-host")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.AcceleratorPreference != "" {
		t.Fatalf("a node nobody chose for replays %q", ds.AcceleratorPreference)
	}
	for _, tc := range []struct{ in, want string }{{" NVIDIA ", "nvidia"}, {"auto", "auto"}, {" AMD ", "amd"}, {"ascend", "ascend"}} {
		if err := s.SetNodeAcceleratorPreference(node.UUID, tc.in); err != nil {
			t.Fatal(err)
		}
		ds, err := s.DesiredStateForNode(node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ds.AcceleratorPreference != tc.want {
			t.Fatalf("after choosing %q the replay says %q", tc.in, ds.AcceleratorPreference)
		}
	}
	for _, bad := range []string{"", "apple", "unknown-vendor", "nvidia,ascend"} {
		if err := s.SetNodeAcceleratorPreference(node.UUID, bad); err == nil {
			t.Fatalf("%q was stored", bad)
		}
	}
	if got, _ := s.NodeByUUID(node.UUID); got.AcceleratorPreference != "ascend" {
		t.Fatalf("a refused choice changed the stored one to %q", got.AcceleratorPreference)
	}
	if err := s.SetNodeAcceleratorPreference("no-such-node", "nvidia"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
}
