package netplan

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func storageInventory(mgmt, storage string) *proto.NetInventory {
	return &proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{mgmt + "/24"}}, {Name: "ens3", Addrs: []string{storage + "/16"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: mgmt},
	}
}

func TestStoragePathBetweenPrefersASharedNetworkThenTheManagementNetwork(t *testing.T) {
	a := Node{ID: 1, Labels: map[string]string{"rack": "r1"}, Inventory: storageInventory("172.25.17.1", "10.30.1.1")}
	b := Node{ID: 2, Labels: map[string]string{"rack": "r1"}, Inventory: storageInventory("172.25.17.2", "10.30.1.2")}
	c := Node{ID: 3, Inventory: storageInventory("172.25.17.3", "10.30.1.3")}
	off := Node{ID: 4, Inventory: storageInventory("172.25.17.4", "10.30.1.4"), RolesOff: rolesOff(RoleStorageSync)}
	storage := network(9, "storage", TypeHostEthernet, "10.30.0.0/16", "", RoleStorageSync)
	in := Inputs{Nodes: []Node{a, b, c, off}, Networks: []Network{storage}, Attachments: []Attachment{{ID: 1, NetworkID: 9, LabelKey: "rack", LabelValue: "r1"}}}
	resolved := Resolve(in)

	if p, err := StoragePathBetween(a, b, resolved); err != nil || p != (StoragePath{Network: "storage", SourceIP: "10.30.1.1", TargetIP: "10.30.1.2"}) {
		t.Fatalf("shared network = %+v, %v", p, err)
	}
	// Only one end is on the storage network: both fall back to management.
	if p, err := StoragePathBetween(a, c, resolved); err != nil || p != (StoragePath{Network: ManagementName, SourceIP: "172.25.17.1", TargetIP: "172.25.17.3"}) {
		t.Fatalf("management fallback = %+v, %v", p, err)
	}
	// The target does not let the management network carry storage sync.
	if _, err := StoragePathBetween(c, off, resolved); !errors.Is(err, ErrNoStoragePath) {
		t.Fatalf("role off err = %v", err)
	}
}

// TestStoragePathUsesAttachedNetworksEndpoints ports the resolution-decision
// part of the equivalent store test: which network and endpoints a copy uses,
// not the DB/persistence path.
func TestStoragePathUsesAttachedNetworksEndpoints(t *testing.T) {
	newNode := func(id uint, octet string) Node {
		return Node{ID: id, Inventory: &proto.NetInventory{
			Interfaces: []proto.NetInterface{
				{Name: "eth0", Addrs: []string{"192.168.8." + octet + "/24"}},
				{Name: "ens3f0", Addrs: []string{"10.30.1." + octet + "/16", "fd00::" + octet + "/64"}},
			},
			Management: &proto.ManagementAddress{Interface: "eth0", IP: "192.168.8." + octet},
		}}
	}
	a, b := newNode(1, "15"), newNode(2, "16")

	// No storage network: the management network carries the copy.
	none := Resolve(Inputs{Nodes: []Node{a, b}})
	if p, err := StoragePathBetween(a, b, none); err != nil || p != (StoragePath{Network: ManagementName, SourceIP: "192.168.8.15", TargetIP: "192.168.8.16"}) {
		t.Fatalf("management path = %+v, %v", p, err)
	}

	storage := network(9, "storage", TypeHostEthernet, "10.30.0.0/16", "", RoleStorageSync)
	withStorage := Resolve(Inputs{Nodes: []Node{a, b}, Networks: []Network{storage}, Attachments: []Attachment{
		{ID: 1, NetworkID: 9, NodeID: 1}, {ID: 2, NetworkID: 9, NodeID: 2},
	}})
	if p, err := StoragePathBetween(a, b, withStorage); err != nil || p != (StoragePath{Network: "storage", SourceIP: "10.30.1.15", TargetIP: "10.30.1.16"}) {
		t.Fatalf("storage path = %+v, %v", p, err)
	}

	// An override to the IPv6 address on one end's attachment is honored.
	overridden := Resolve(Inputs{Nodes: []Node{a, b}, Networks: []Network{storage}, Attachments: []Attachment{
		{ID: 1, NetworkID: 9, NodeID: 1, Interface: "ens3f0", IP: "fd00::15"}, {ID: 2, NetworkID: 9, NodeID: 2},
	}})
	if p, err := StoragePathBetween(a, b, overridden); err != nil || p.SourceIP != "fd00::15" {
		t.Fatalf("ipv6 override = %+v, %v", p, err)
	}
}

func TestCanServe(t *testing.T) {
	servingNet := network(5, "svc", TypeHostEthernet, "10.40.0.0/24", "", RoleServing)
	withOwn := Node{ID: 1, RolesOff: rolesOff(RoleServing), Inventory: &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}}}}
	withoutOwn := Node{ID: 2, RolesOff: rolesOff(RoleServing)}
	roleOn := Node{ID: 3}
	resolved := Resolve(Inputs{
		Nodes:       []Node{withOwn, withoutOwn, roleOn},
		Networks:    []Network{servingNet},
		Attachments: []Attachment{{ID: 1, NetworkID: 5, NodeID: 1}},
	})
	if !CanServe(withOwn, resolved) {
		t.Fatal("node with its own serving network cannot serve")
	}
	if CanServe(withoutOwn, resolved) {
		t.Fatal("node with serving off and no network of its own can serve")
	}
	if !CanServe(roleOn, resolved) {
		t.Fatal("node with serving left on cannot serve over the management network")
	}
}

// TestCanServeManagementSourceWithRoleOffIsNotAllowed exercises the branch
// where a node resolved a management-sourced serving network (because serving
// was on when Resolve ran) but is asked about with RolesOff now set: CanServe
// must not let that count as its own network.
func TestCanServeManagementSourceWithRoleOffIsNotAllowed(t *testing.T) {
	node := Node{ID: 1, Inventory: &proto.NetInventory{Management: &proto.ManagementAddress{IP: "10.0.0.1"}}}
	resolved := Resolve(Inputs{Nodes: []Node{node}})
	off := node
	off.RolesOff = rolesOff(RoleServing)
	if CanServe(off, resolved) {
		t.Fatal("a role turned off over a management-sourced resolution should not serve")
	}
}

func TestServingIP(t *testing.T) {
	svc := network(5, "svc", TypeHostEthernet, "10.40.0.0/24", "", RoleServing)
	node := Node{ID: 1, Inventory: &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}}}}
	resolved := Resolve(Inputs{Nodes: []Node{node}, Networks: []Network{svc}, Attachments: []Attachment{{ID: 1, NetworkID: 5, NodeID: 1}}})
	if ip := ServingIP(1, resolved); ip != "10.40.0.5" {
		t.Fatalf("serving ip = %q", ip)
	}

	// Serving over the management network rides the agent tunnel: no forwarder IP.
	mgmtNode := Node{ID: 2, Inventory: &proto.NetInventory{Management: &proto.ManagementAddress{IP: "10.0.0.9"}}}
	mgmtResolved := Resolve(Inputs{Nodes: []Node{mgmtNode}})
	if ip := ServingIP(2, mgmtResolved); ip != "" {
		t.Fatalf("management-sourced serving got a forwarder ip: %q", ip)
	}

	if ip := ServingIP(999, resolved); ip != "" {
		t.Fatalf("missing node serving ip = %q", ip)
	}
}

// ServingIPs answers what ServingIP answers for each node, from one pass.
func TestServingIPsMatchesServingIP(t *testing.T) {
	svc := network(5, "svc", TypeHostEthernet, "10.40.0.0/24", "", RoleServing)
	nodes := []Node{
		{ID: 1, Inventory: &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}}}},
		{ID: 2, Inventory: &proto.NetInventory{Management: &proto.ManagementAddress{IP: "10.0.0.9"}}},
		{ID: 3, Inventory: &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.7/24"}}}}},
	}
	resolved := Resolve(Inputs{Nodes: nodes, Networks: []Network{svc}, Attachments: []Attachment{{ID: 1, NetworkID: 5, NodeID: 1}, {ID: 2, NetworkID: 5, NodeID: 3}}})
	ips := ServingIPs(resolved)
	for _, node := range append(nodes, Node{ID: 999}) {
		if got, want := ips[node.ID], ServingIP(node.ID, resolved); got != want {
			t.Fatalf("node %d: ServingIPs = %q, ServingIP = %q", node.ID, got, want)
		}
	}
	if len(ips) != 2 || ips[1] != "10.40.0.5" || ips[3] != "10.40.0.7" {
		t.Fatalf("ips = %v", ips)
	}
}
