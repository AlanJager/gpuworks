package network

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func acceleratorInventory(mgmt string, cards ...int) proto.NetInventory {
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", State: "up", Addrs: []string{mgmt + "/24"}}},
		RDMAPorts:  []proto.RDMAPort{{Device: "rocep61s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 25, NetDev: "eth0"}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: mgmt},
	}
	for _, i := range cards {
		inv.AcceleratorPorts = append(inv.AcceleratorPorts, proto.AcceleratorPort{
			Name: "npu" + string(rune('0'+i)), Index: i, IP: "172.20.9." + string(rune('0'+i)), Netmask: "255.255.0.0", State: "UP", SpeedGbps: 200,
		})
	}
	return inv
}

func TestAcceleratorNetworkCarriesComputeTrafficOnly(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	for _, roles := range [][]string{
		{netplan.RoleStorageSync},
		{netplan.RoleCompute, netplan.RoleServing},
	} {
		_, err := svc.CreateNetwork(NetworkSpec{Name: "npu", Roles: roles, Type: netplan.TypeAcceleratorRDMA})
		want := roles[len(roles)-1]
		assertInvalid(t, err, InvalidTypeRole, netplan.TypeAcceleratorRDMA, want)
	}
	// Like a host RDMA network it takes no interface glob, only a CIDR for
	// the bootstrap.
	_, err := svc.CreateNetwork(NetworkSpec{Name: "npu", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeAcceleratorRDMA, Interface: "eth*"})
	assertInvalid(t, err, InvalidSelector, "interface", "eth*")

	n, err := svc.CreateNetwork(NetworkSpec{Name: "npu", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeAcceleratorRDMA, CIDR: "10.0.0.9/24"})
	if err != nil {
		t.Fatalf("create: %+v", err)
	}
	if n.Type != "accelerator_rdma" || n.CIDR != "10.0.0.0/24" || n.Roles != `["compute"]` {
		t.Fatalf("stored network = %+v", n)
	}
	// Changing an existing network to the type is held to the same rule.
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "lan", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpdateNetwork(eth.ID, NetworkSpec{Name: "lan", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeAcceleratorRDMA})
	assertInvalid(t, err, InvalidTypeRole, netplan.TypeAcceleratorRDMA, netplan.RoleStorageSync)
}

func TestAcceleratorNetworkRailsArePinnedByCard(t *testing.T) {
	st := newStore(t)
	svc := New(st, StoreCatalog{Store: st})
	n, err := svc.CreateNetwork(NetworkSpec{Name: "npu", Roles: []string{netplan.RoleCompute}, Type: netplan.TypeAcceleratorRDMA})
	if err != nil {
		t.Fatal(err)
	}
	for uuid, mgmt := range map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		reportInventory(t, st, uuid, acceleratorInventory(mgmt, 3, 4, 5))
		if _, err := svc.Attach(n.ID, AttachmentSpec{NodeUUID: uuid}); err != nil {
			t.Fatalf("attach %s: %+v", uuid, err)
		}
	}
	rails := func(uuid string) ([]string, bool) {
		t.Helper()
		state, err := svc.State()
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range state.Nodes {
			if node.UUID == uuid {
				r, _ := state.Resolved.For(node.ID, netplan.RoleCompute)
				return r.HCAs, r.HCAsPinned
			}
		}
		t.Fatalf("node %s not found", uuid)
		return nil, false
	}
	if got, pinned := rails("node-a"); !reflect.DeepEqual(got, []string{"npu3", "npu4", "npu5"}) || pinned {
		t.Fatalf("inferred rails = %v pinned=%v", got, pinned)
	}

	// Removing a card without naming a node removes it everywhere.
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCARemove, HCA: "npu5"}); err != nil {
		t.Fatalf("remove: %+v", err)
	}
	for _, uuid := range []string{"node-a", "node-b"} {
		if got, pinned := rails(uuid); !reflect.DeepEqual(got, []string{"npu3", "npu4"}) || !pinned {
			t.Fatalf("%s rails after remove = %v pinned=%v", uuid, got, pinned)
		}
	}
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-a", HCA: "npu5"}); err != nil {
		t.Fatalf("add: %+v", err)
	}
	if got, _ := rails("node-a"); !reflect.DeepEqual(got, []string{"npu3", "npu4", "npu5"}) {
		t.Fatalf("rails after add = %v", got)
	}
	// A card the node does not have, a host RDMA device, and a prefix of a
	// card name are all refused.
	for _, hca := range []string{"npu9", "rocep61s0f0", "rocep61s0f0:1", "npu"} {
		if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAAdd, NodeUUID: "node-a", HCA: hca}); !errors.Is(err, ErrHCANotOnNode) {
			t.Errorf("add %q: err = %v, want ErrHCANotOnNode", hca, err)
		}
	}
	if err := svc.EditHCAs(n.ID, HCAEdit{Op: HCAReset, NodeUUID: "node-b"}); err != nil {
		t.Fatalf("reset: %+v", err)
	}
	if got, pinned := rails("node-b"); !reflect.DeepEqual(got, []string{"npu3", "npu4", "npu5"}) || pinned {
		t.Fatalf("rails after reset = %v pinned=%v", got, pinned)
	}
	// An attachment may pin cards from the start; a card name on a host
	// Ethernet network is still refused.
	eth, err := svc.CreateNetwork(NetworkSpec{Name: "lan", Roles: []string{netplan.RoleStorageSync}, Type: netplan.TypeHostEthernet})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Attach(eth.ID, AttachmentSpec{NodeUUID: "node-a", Overrides: Overrides{HCAs: []string{"npu3"}}})
	assertInvalid(t, err, InvalidOverride, "hcas", "npu3")
}
