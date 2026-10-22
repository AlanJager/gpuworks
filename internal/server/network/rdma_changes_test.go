package network

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// roceInventory is a two-rail RoCE node plus a 1G RoCE port with no address,
// shaped like the Ascend lab hosts.
func roceInventory(last string) proto.NetInventory {
	return proto.NetInventory{
		Interfaces: []proto.NetInterface{
			{Name: "eth0", SpeedMbps: 1000, Addrs: []string{"10.1.0." + last + "/24"}},
			{Name: "enp61s0f0", SpeedMbps: 25000, Addrs: []string{"100.64.29." + last + "/24"}},
			{Name: "enp61s0f2", SpeedMbps: 25000, Addrs: []string{"100.64.30." + last + "/24"}},
			{Name: "enp189s0f0", SpeedMbps: 1000},
		},
		RDMAPorts: []proto.RDMAPort{
			{Device: "rocep189s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp189s0f0"},
			{Device: "rocep61s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp61s0f0"},
			{Device: "rocep61s0f2", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp61s0f2"},
		},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "10.1.0." + last},
	}
}

// rdmaFixture is an RDMA network with two RoCE nodes attached.
func rdmaFixture(t *testing.T) (*Service, *store.Store, *store.Network) {
	t.Helper()
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "fabric", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"node-a", "node-b"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		reportInventory(t, st, uuid, roceInventory([]string{"1", "2"}[i]))
	}
	results, err := svc.AttachNodes(n.ID, []string{"node-a", "node-b"})
	if err != nil || len(results) != 2 {
		t.Fatalf("attach = %+v, %v", results, err)
	}
	return svc, st, n
}

// railsOf is what the network resolves to on a node: its rails and whether
// they are pinned.
func railsOf(t *testing.T, svc *Service, networkID uint, uuid string) ([]string, bool) {
	t.Helper()
	st, err := svc.State()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range st.Nodes {
		if n.UUID != uuid {
			continue
		}
		for _, r := range st.Resolved {
			if r.NodeID == n.ID && r.Network.ID == networkID {
				return r.HCAs, r.HCAsPinned
			}
		}
	}
	t.Fatalf("%s does not resolve network %d", uuid, networkID)
	return nil, false
}

func TestAttachNodesIsPerNode(t *testing.T) {
	svc, _, n := rdmaFixture(t)
	results, err := svc.AttachNodes(n.ID, []string{"node-a", " ", "missing", "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0] != (AttachResult{NodeUUID: "node-a", Status: AttachAlready}) {
		t.Fatalf("results = %+v", results)
	}
	missing := results[1]
	var invalid *InvalidError
	if missing.NodeUUID != "missing" || missing.Status != AttachFailed || !errors.As(missing.Err, &invalid) || invalid.Kind != InvalidTarget {
		t.Fatalf("missing node = %+v", missing)
	}
	if _, err := svc.AttachNodes(999, []string{"node-a"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown network err = %v", err)
	}
	if _, err := New(newStore(t), ManagementOnly{}).AttachNodes(n.ID, nil); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("management-only err = %v", err)
	}
}

func TestEditHCAsPinsAndRemovesAcrossNodes(t *testing.T) {
	svc, _, n := rdmaFixture(t)
	auto := []string{"rocep61s0f0:1", "rocep61s0f2:1"}
	if got, pinned := railsOf(t, svc, n.ID, "node-a"); !reflect.DeepEqual(got, auto) || pinned {
		t.Fatalf("inferred = %v pinned=%v", got, pinned)
	}

	// Adding pins node a's current rails plus the new one; node b stays inferred.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-a", HCA: "rocep189s0f0"}); err != nil {
		t.Fatal(err)
	}
	if got, pinned := railsOf(t, svc, n.ID, "node-a"); !reflect.DeepEqual(got, append(auto, "rocep189s0f0:1")) || !pinned {
		t.Fatalf("after add = %v pinned=%v", got, pinned)
	}
	if _, pinned := railsOf(t, svc, n.ID, "node-b"); pinned {
		t.Fatal("add pinned another node")
	}
	// Adding a rail already there changes nothing.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-b", HCA: "rocep61s0f0:1"}); err != nil {
		t.Fatal(err)
	}
	if _, pinned := railsOf(t, svc, n.ID, "node-b"); pinned {
		t.Fatal("a no-op add pinned the node")
	}

	// Removing without a node takes the port off every node using it.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCARemove, HCA: "rocep61s0f2:1"}); err != nil {
		t.Fatal(err)
	}
	a, _ := railsOf(t, svc, n.ID, "node-a")
	b, bPinned := railsOf(t, svc, n.ID, "node-b")
	if !reflect.DeepEqual(a, []string{"rocep61s0f0:1", "rocep189s0f0:1"}) || !reflect.DeepEqual(b, []string{"rocep61s0f0:1"}) || !bPinned {
		t.Fatalf("after remove: a=%v b=%v", a, b)
	}

	// Removing from one node leaves the other alone.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCARemove, HCA: "rocep189s0f0", NodeUUID: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := railsOf(t, svc, n.ID, "node-a"); !reflect.DeepEqual(a, []string{"rocep61s0f0:1"}) {
		t.Fatalf("node a = %v", a)
	}

	// The last rail of a node cannot go, and nothing changes on any node.
	var last *LastRailError
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCARemove, HCA: "rocep61s0f0:1"}); !errors.As(err, &last) {
		t.Fatalf("last rail err = %v", err)
	}
	if b, _ := railsOf(t, svc, n.ID, "node-b"); !reflect.DeepEqual(b, []string{"rocep61s0f0:1"}) {
		t.Fatalf("refused edit changed node b: %v", b)
	}

	// Reset lets the node's rails be inferred again.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAReset, NodeUUID: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if got, pinned := railsOf(t, svc, n.ID, "node-a"); !reflect.DeepEqual(got, auto) || pinned {
		t.Fatalf("after reset = %v pinned=%v", got, pinned)
	}
}

func TestEditHCAsOnLabelAttachedNodeCreatesNodeAttachment(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "fabric", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeHostRDMA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-l", store.HashToken("l"), "l"); err != nil {
		t.Fatal(err)
	}
	reportInventory(t, st, "node-l", roceInventory("9"))
	if _, err := st.SetNodeLabels("node-l", map[string]string{"pool": "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Attach(n.ID, AttachmentSpec{LabelKey: "pool", LabelValue: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCARemove, HCA: "rocep61s0f2:1"}); err != nil {
		t.Fatal(err)
	}
	if got, pinned := railsOf(t, svc, n.ID, "node-l"); !reflect.DeepEqual(got, []string{"rocep61s0f0:1"}) || !pinned {
		t.Fatalf("label node = %v pinned=%v", got, pinned)
	}
	atts, _ := st.ListNetworkAttachments()
	if len(atts) != 2 || atts[1].NodeID == 0 {
		t.Fatalf("attachments = %+v", atts)
	}
	// Reset on a node that only has its label attachment is a no-op.
	if err := st.DeleteNetworkAttachment(n.ID, atts[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAReset, NodeUUID: "node-l"}); err != nil {
		t.Fatal(err)
	}
	if atts, _ := st.ListNetworkAttachments(); len(atts) != 1 {
		t.Fatalf("reset created an attachment: %+v", atts)
	}
}

func TestEditHCAsRefusals(t *testing.T) {
	svc, st, n := rdmaFixture(t)
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "lan", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-x", store.HashToken("x"), "x"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		network uint
		edit    HCAEdit
		want    error
	}{
		"ethernet":        {eth.ID, HCAEdit{Op: HCARemove, HCA: "x"}, ErrNotRDMA},
		"missing network": {999, HCAEdit{Op: HCARemove, HCA: "x"}, store.ErrNotFound},
		"node off net":    {n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-x", HCA: "rocep61s0f0"}, ErrNodeNotOnNetwork},
		"unknown node":    {n.ID, HCAEdit{Op: HCAReset, NodeUUID: "nope"}, ErrNodeNotOnNetwork},
		"remove off net":  {n.ID, HCAEdit{Op: HCARemove, NodeUUID: "node-x", HCA: "rocep61s0f0"}, ErrNodeNotOnNetwork},
		"unreported port": {n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-a", HCA: "mlx5_0"}, ErrHCANotOnNode},
		"bad port name":   {n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-a", HCA: "a b"}, ErrHCANotOnNode},
		"unused port":     {n.ID, HCAEdit{Op: HCARemove, HCA: "rocep189s0f0:1"}, ErrHCANotUsed},
	} {
		if err := svc.EditHCAs(c.network, c.edit); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	assertInvalid(t, svc.EditHCAs(n.ID, HCAEdit{Op: "swap"}), InvalidOverride, "op", "swap")
	if err := New(st, ManagementOnly{}).EditHCAs(n.ID, HCAEdit{Op: HCAReset}); !errors.Is(err, ErrDedicatedUnavailable) {
		t.Fatalf("management-only err = %v", err)
	}
}
