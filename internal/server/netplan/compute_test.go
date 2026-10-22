package netplan

import (
	"errors"
	"testing"
)

func TestComputeGroupSharesANetworkOrFallsBackToManagement(t *testing.T) {
	ib := network(7, "ib", TypeHostEthernet, "10.96.0.0/13", "", RoleCompute)
	ib2 := network(8, "ib2", TypeHostEthernet, "10.96.0.0/13", "", RoleCompute)
	names := map[uint]string{1: "a", 2: "b", 3: "c"}
	node := func(id uint, labels map[string]string, mgmt string, off ...string) Node {
		return Node{ID: id, Name: names[id], Labels: labels, Inventory: managedInventory(mgmt, "eth0"), RolesOff: rolesOff(off...)}
	}
	attach := []Attachment{
		{ID: 1, NetworkID: 7, LabelKey: "pool", LabelValue: "ib"},
		{ID: 2, NetworkID: 8, LabelKey: "pool", LabelValue: "ib2"},
	}
	group := func(nodes ...Node) (Group, error) {
		return ComputeGroup(nodes, Resolve(Inputs{Nodes: nodes, Networks: []Network{ib, ib2}, Attachments: attach}))
	}

	// Every rank on one compute network: that network.
	g, err := group(node(1, map[string]string{"pool": "ib"}, "172.25.0.1"), node(2, map[string]string{"pool": "ib"}, "172.25.0.2"))
	if err != nil || g.Network.ID != 7 || g.FellBack || g.Ranks[1].IP != "10.96.0.5" {
		t.Fatalf("shared = %+v, %v", g, err)
	}
	// One rank is not on it: the whole group falls back to management.
	g, err = group(node(1, map[string]string{"pool": "ib"}, "172.25.0.1"), node(2, nil, "172.25.0.2"))
	if err != nil || g.Network.Name != ManagementName || !g.FellBack || g.Ranks[1].IP != "172.25.0.1" || g.Ranks[2].IP != "172.25.0.2" {
		t.Fatalf("fallback = %+v, %v", g, err)
	}
	// No rank has a network of its own: management, and no fallback to warn about.
	g, err = group(node(1, nil, "172.25.0.1"), node(2, nil, "172.25.0.2"))
	if err != nil || g.Network.Name != ManagementName || g.FellBack {
		t.Fatalf("management = %+v, %v", g, err)
	}

	var ge *GroupError
	// A rank that turned compute traffic off on management and has no network.
	_, err = group(node(1, nil, "172.25.0.1"), node(2, nil, "172.25.0.2", RoleCompute))
	if !errors.As(err, &ge) || ge.Reason != ComputeNone || ge.Node.ID != 2 {
		t.Fatalf("none = %v", err)
	}
	if ge.Error() == "" {
		t.Fatal("empty message for ComputeNone")
	}

	// On ib, but management may not carry it, and the other node is on no network.
	_, err = group(node(1, map[string]string{"pool": "ib"}, "172.25.0.1", RoleCompute), node(2, nil, "172.25.0.2"))
	if !errors.As(err, &ge) || ge.Reason != ComputeMixed || ge.Node.ID != 1 || ge.Other.ID != 2 {
		t.Fatalf("mixed = %v", err)
	}
	if ge.Error() == "" {
		t.Fatal("empty message for ComputeMixed")
	}

	// Two different compute networks and no management to share.
	_, err = group(node(1, map[string]string{"pool": "ib"}, "172.25.0.1", RoleCompute), node(2, map[string]string{"pool": "ib2"}, "172.25.0.2"))
	if !errors.As(err, &ge) || ge.Reason != ComputeMismatch || ge.Network != "ib" || ge.Expected != "ib2" {
		t.Fatalf("mismatch = %v", err)
	}
	if ge.Error() == "" {
		t.Fatal("empty message for ComputeMismatch")
	}
}

func TestComputeGroupWithNoNodes(t *testing.T) {
	g, err := ComputeGroup(nil, nil)
	if err != nil || g.Network.Name != ManagementName || g.FellBack || len(g.Ranks) != 0 {
		t.Fatalf("empty group = %+v, %v", g, err)
	}
}
