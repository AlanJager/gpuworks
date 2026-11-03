package netplan

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// h20Inventory is an 8-rail IB host with a bonded bootstrap interface and a
// RoCE storage NIC. It never reports a management address, like an older
// agent.
func h20Inventory() *proto.NetInventory {
	return &proto.NetInventory{
		Interfaces: []proto.NetInterface{
			{Name: "bond0", State: "up", Addrs: []string{"10.20.1.15/16"}},
			{Name: "ens3f0", State: "up", Addrs: []string{"fd00::15/64", "10.30.1.15/16"}},
			{Name: "eth0", State: "up", Addrs: []string{"192.168.8.15/24"}},
		},
		RDMAPorts: []proto.RDMAPort{
			{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400},
			{Device: "mlx5_1", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400},
			{Device: "mlx5_2", Port: 1, State: "DOWN", LinkLayer: "InfiniBand", RateGbps: 400},
			{Device: "mlx5_bond_0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 25, NetDev: "ens3f0"},
		},
	}
}

// managedInventory is a node that reports a management address on iface, plus
// an RDMA-capable ib0 interface in the 10.96.0.0/13 range.
func managedInventory(ip, iface string) *proto.NetInventory {
	return &proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: iface, Addrs: []string{ip + "/24"}}, {Name: "ib0", Addrs: []string{"10.96.0.5/13"}}},
		Management: &proto.ManagementAddress{Interface: iface, IP: ip},
	}
}

func network(id uint, name, typ, cidr, iface string, roles ...string) Network {
	return Network{ID: id, Name: name, Type: typ, CIDR: cidr, Interface: iface, Roles: roles}
}

func rolesOff(roles ...string) map[string]bool {
	m := map[string]bool{}
	for _, r := range roles {
		m[r] = true
	}
	return m
}

func find(t *testing.T, out Resolution, nodeID uint, role string) Resolved {
	t.Helper()
	for _, r := range out {
		if r.NodeID == nodeID && r.Role == role {
			return r
		}
	}
	t.Fatalf("node %d has no %s network in %#v", nodeID, role, out)
	return Resolved{}
}

func absent(t *testing.T, out Resolution, nodeID uint, role string) {
	t.Helper()
	for _, r := range out {
		if r.NodeID == nodeID && r.Role == role {
			t.Fatalf("node %d unexpectedly resolved %s to %s", nodeID, role, r.Network.Name)
		}
	}
}

func TestResolvePicksEndpointsFromSelectors(t *testing.T) {
	inv := h20Inventory()
	in := Inputs{
		Nodes: []Node{{ID: 1, Labels: map[string]string{"pool": "h20-a"}, Inventory: inv}},
		Networks: []Network{
			network(10, "ib-a", TypeHostRDMA, "10.20.0.0/16", "", RoleCompute),
			network(11, "storage", TypeHostEthernet, "10.30.0.0/16", "ens*", RoleStorageSync, RoleServing),
		},
		Attachments: []Attachment{
			{ID: 1, NetworkID: 10, LabelKey: "pool", LabelValue: "h20-a"},
			{ID: 2, NetworkID: 11, LabelKey: "pool", LabelValue: "h20-a"},
		},
	}
	out := Resolve(in)
	compute := find(t, out, 1, RoleCompute)
	if compute.Interface != "bond0" || compute.IP != "10.20.1.15" || compute.Source != SourceLabel || compute.LabelKey != "pool" {
		t.Fatalf("compute = %#v", compute)
	}
	// Only active InfiniBand ports: the down rail and the RoCE bond drop out.
	if want := []string{"mlx5_0:1", "mlx5_1:1"}; !reflect.DeepEqual(compute.HCAs, want) {
		t.Fatalf("HCAs = %v, want %v", compute.HCAs, want)
	}
	storage := find(t, out, 1, RoleStorageSync)
	if storage.Interface != "ens3f0" || storage.IP != "10.30.1.15" || storage.HCAs != nil || len(storage.Warnings) != 0 {
		t.Fatalf("storage = %#v", storage)
	}
	if serving := find(t, out, 1, RoleServing); serving.Network.ID != 11 {
		t.Fatalf("serving = %#v", serving)
	}
}

func TestResolveRanksNodeAboveLabelAndFlagsSameRankConflicts(t *testing.T) {
	inv := h20Inventory()
	nodes := []Node{
		{ID: 1, Labels: map[string]string{"pool": "h20-a", "site": "lab"}, Inventory: inv},
		{ID: 2, Labels: map[string]string{"pool": "h20-a", "site": "lab"}, Inventory: inv},
		{ID: 3, Labels: map[string]string{"site": "other"}, Inventory: inv},
	}
	networks := []Network{
		network(10, "ib-a", TypeHostRDMA, "10.20.0.0/16", "", RoleCompute),
		network(11, "legacy", TypeHostEthernet, "", "eth0", RoleCompute, RoleStorageSync),
		network(12, "orphan", TypeHostEthernet, "", "eth0", RoleCompute),
	}
	attachments := []Attachment{
		{ID: 1, NetworkID: 10, LabelKey: "pool", LabelValue: "h20-a"},
		{ID: 2, NetworkID: 11, LabelKey: "site", LabelValue: "lab"},
		{ID: 3, NetworkID: 11, NodeID: 2}, // node 2 pins legacy; also reached by label, counted once
		{ID: 4, NetworkID: 99, NodeID: 1}, // dangling network id is ignored
	}
	out := Resolve(Inputs{Nodes: nodes, Networks: networks, Attachments: attachments})

	first := find(t, out, 1, RoleCompute)
	wantConflict := []Warning{{Code: WarningConflict, Params: map[string]string{"role": "compute", "winner": "ib-a", "losers": "legacy"}}}
	if first.Network.Name != "ib-a" || !reflect.DeepEqual(first.Warnings, wantConflict) {
		t.Fatalf("node 1 compute = %#v", first)
	}
	pinned := find(t, out, 2, RoleCompute)
	if pinned.Network.Name != "legacy" || pinned.Source != SourceNode || len(pinned.Warnings) != 0 || pinned.Interface != "eth0" || pinned.IP != "192.168.8.15" {
		t.Fatalf("node 2 compute = %#v", pinned)
	}
	if got := find(t, out, 2, RoleStorageSync); got.Source != SourceNode || got.AttachmentID != 3 {
		t.Fatalf("node 2 storage = %#v", got)
	}
	// Node 3's inventory has no management address, so a role nothing reaches
	// it for stays absent instead of falling back.
	absent(t, out, 3, RoleCompute)
	absent(t, out, 1, RoleServing)
}

func TestResolveAppliesAndChecksOverrides(t *testing.T) {
	inv := h20Inventory()
	nodes := []Node{
		{ID: 1, Inventory: inv},
		{ID: 2, Inventory: inv},
		{ID: 3}, // never reported an inventory: overrides are trusted as written
	}
	networks := []Network{network(10, "ib-a", TypeHostRDMA, "10.20.0.0/16", "", RoleCompute)}
	attachments := []Attachment{
		{ID: 1, NetworkID: 10, NodeID: 1, Interface: "eth0", HCAs: []string{"mlx5_1", "mlx5_0:1"}},
		{ID: 2, NetworkID: 10, NodeID: 2, IP: "10.99.0.1", HCAs: []string{"mlx5_7", "mlx5_8"}},
		{ID: 3, NetworkID: 10, NodeID: 3, Interface: "ib0", IP: "10.20.9.9", HCAs: []string{"mlx5_3", "mlx5_4"}},
	}
	out := Resolve(Inputs{Nodes: nodes, Networks: networks, Attachments: attachments})

	one := find(t, out, 1, RoleCompute)
	if one.Interface != "eth0" || one.IP != "192.168.8.15" || !reflect.DeepEqual(one.HCAs, []string{"mlx5_1:1", "mlx5_0:1"}) || len(one.Warnings) != 0 {
		t.Fatalf("node 1 = %#v", one)
	}
	if !one.HCAsPinned || !one.BootstrapPinned {
		t.Fatalf("node 1 pins = %v %v", one.HCAsPinned, one.BootstrapPinned)
	}
	two := find(t, out, 2, RoleCompute)
	wantWarnings := []Warning{
		{Code: WarningOverrideMissing, Params: map[string]string{"kind": "hca", "value": "mlx5_7, mlx5_8"}},
		{Code: WarningOverrideMissing, Params: map[string]string{"kind": "interface", "value": "10.99.0.1"}},
	}
	if two.IP != "10.99.0.1" || !reflect.DeepEqual(two.Warnings, wantWarnings) {
		t.Fatalf("node 2 = %#v", two)
	}
	three := find(t, out, 3, RoleCompute)
	if three.Interface != "ib0" || three.IP != "10.20.9.9" || !reflect.DeepEqual(three.HCAs, []string{"mlx5_3", "mlx5_4"}) || len(three.Warnings) != 0 {
		t.Fatalf("node 3 = %#v", three)
	}
}

// TestResolveOverrideMatchesByIPOnly exercises findOverride's ip-only branch
// (no Interface override) actually finding an address, not just failing to.
func TestResolveOverrideMatchesByIPOnly(t *testing.T) {
	inv := h20Inventory()
	nodes := []Node{{ID: 1, Inventory: inv}}
	networks := []Network{network(10, "ib-a", TypeHostRDMA, "10.20.0.0/16", "", RoleCompute)}
	attachments := []Attachment{{ID: 1, NetworkID: 10, NodeID: 1, IP: "192.168.8.15"}}
	out := Resolve(Inputs{Nodes: nodes, Networks: networks, Attachments: attachments})
	got := find(t, out, 1, RoleCompute)
	if got.Interface != "eth0" || got.IP != "192.168.8.15" || len(got.Warnings) != 0 {
		t.Fatalf("got = %#v", got)
	}
}

func TestResolveSelectorEdgeCases(t *testing.T) {
	inv := h20Inventory()
	nodes := []Node{{ID: 1, Inventory: inv}}
	cases := []struct {
		name          string
		network       Network
		wantInterface string
		wantIP        string
		wantHCAs      []string
	}{
		{"cidr matches no interface", network(1, "n", TypeHostEthernet, "172.16.0.0/12", ""), "", "", nil},
		{"glob without cidr takes first ipv4", network(1, "n", TypeHostEthernet, "", "ens*"), "ens3f0", "10.30.1.15", nil},
		{"glob and cidr intersect", network(1, "n", TypeHostEthernet, "10.20.0.0/16", "eth*"), "", "", nil},
		{"empty selector picks nothing", network(1, "n", TypeHostEthernet, "", ""), "", "", nil},
		{"invalid cidr ignored with glob", network(1, "n", TypeHostEthernet, "nope", "eth0"), "eth0", "192.168.8.15", nil},
		{"rdma prefers active infiniband ports over a roce nic", network(1, "n", TypeHostRDMA, "10.30.0.0/16", ""), "ens3f0", "10.30.1.15", []string{"mlx5_0:1", "mlx5_1:1"}},
		// An RDMA network has no interface glob: only its CIDR picks the bootstrap.
		{"rdma ignores an interface glob", network(1, "n", TypeHostRDMA, "10.30.0.0/16", "eth*"), "ens3f0", "10.30.1.15", []string{"mlx5_0:1", "mlx5_1:1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.network.Roles = []string{RoleCompute}
			out := Resolve(Inputs{Nodes: nodes, Networks: []Network{tc.network}, Attachments: []Attachment{{ID: 1, NetworkID: 1, NodeID: 1}}})
			got := find(t, out, 1, RoleCompute)
			if got.Interface != tc.wantInterface || got.IP != tc.wantIP || !reflect.DeepEqual(got.HCAs, tc.wantHCAs) {
				t.Fatalf("got %q %q %v, want %q %q %v", got.Interface, got.IP, got.HCAs, tc.wantInterface, tc.wantIP, tc.wantHCAs)
			}
		})
	}
}

// TestResolveMatchInterfaceFallsBackToIPv6WithoutIPv4 exercises pickAddress's
// fallback branch (no CIDR, only an IPv6 address available) and its
// malformed-address skip branch (a garbage entry ahead of the real one).
func TestResolveMatchInterfaceFallsBackToIPv6WithoutIPv4(t *testing.T) {
	inv := &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "ib0", Addrs: []string{"not-a-cidr", "fd00::99/64"}}}}
	nodes := []Node{{ID: 1, Inventory: inv}}
	n := network(1, "n", TypeHostEthernet, "", "ib*", RoleCompute)
	out := Resolve(Inputs{Nodes: nodes, Networks: []Network{n}, Attachments: []Attachment{{ID: 1, NetworkID: 1, NodeID: 1}}})
	got := find(t, out, 1, RoleCompute)
	if got.Interface != "ib0" || got.IP != "fd00::99" {
		t.Fatalf("got %#v", got)
	}
}

// TestResolveNilInventoryAndLabels is what a corrupt inventory/labels column
// decoded to before decoding moved out of this package: nil, not zero values
// that panic on lookup.
func TestResolveNilInventoryAndLabels(t *testing.T) {
	nodes := []Node{{ID: 1}} // zero-value: nil Labels, nil Inventory
	networks := []Network{network(1, "n", TypeHostEthernet, "10.0.0.0/8", "", RoleStorageSync)}
	out := Resolve(Inputs{Nodes: nodes, Networks: networks, Attachments: []Attachment{
		{ID: 1, NetworkID: 1, LabelKey: "a", LabelValue: "b"},
		{ID: 2, NetworkID: 1, NodeID: 1},
	}})
	got := find(t, out, 1, RoleStorageSync)
	if got.Interface != "" || got.Source != SourceNode {
		t.Fatalf("got %#v", got)
	}
}

func TestResolveDetectsTheLinkLayer(t *testing.T) {
	ibHost := h20Inventory()
	roceHost := &proto.NetInventory{
		Interfaces: []proto.NetInterface{
			{Name: "bond0", Addrs: []string{"10.20.1.30/16"}},
			{Name: "ens48f0np0", SpeedMbps: 200000, Addrs: []string{"10.50.0.1/31"}},
			{Name: "ens48f1np1", SpeedMbps: 200000, Addrs: []string{"10.50.0.3/31"}},
		},
		RDMAPorts: []proto.RDMAPort{
			{Device: "mlx5_2", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 200, NetDev: "ens48f0np0"},
			{Device: "mlx5_3", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 200, NetDev: "ens48f1np1"},
		},
	}
	nodes := []Node{{ID: 1, Inventory: ibHost}, {ID: 2, Inventory: roceHost}, {ID: 3, Inventory: ibHost}}
	rdma := network(1, "fabric", TypeHostRDMA, "10.20.0.0/16", "", RoleCompute)
	eth := network(2, "lan", TypeHostEthernet, "10.30.0.0/16", "", RoleStorageSync)

	// Two InfiniBand nodes: detected, no warning.
	out := Resolve(Inputs{Nodes: nodes, Networks: []Network{rdma, eth}, Attachments: []Attachment{
		{ID: 1, NetworkID: 1, NodeID: 1}, {ID: 2, NetworkID: 1, NodeID: 3}, {ID: 3, NetworkID: 2, NodeID: 1},
	}})
	if got := find(t, out, 1, RoleCompute); got.LinkLayer != LinkLayerInfiniBand || len(got.Warnings) != 0 {
		t.Fatalf("ib node = %#v", got)
	}
	if got := find(t, out, 1, RoleStorageSync); got.LinkLayer != "" || got.HCAs != nil {
		t.Fatalf("ethernet network carried rdma facts: %#v", got)
	}

	// Adding a RoCE node to the same network flags every member.
	out = Resolve(Inputs{Nodes: nodes, Networks: []Network{rdma}, Attachments: []Attachment{
		{ID: 1, NetworkID: 1, NodeID: 1}, {ID: 2, NetworkID: 1, NodeID: 2},
	}})
	want := []Warning{{Code: WarningLinkLayerMismatch, Params: map[string]string{"link_layers": "infiniband, roce"}}}
	for _, id := range []uint{1, 2} {
		got := find(t, out, id, RoleCompute)
		if !reflect.DeepEqual(got.Warnings, want) {
			t.Fatalf("node %d warnings = %#v", id, got.Warnings)
		}
	}
	if got := find(t, out, 2, RoleCompute); got.LinkLayer != LinkLayerRoCE || !reflect.DeepEqual(got.HCAs, []string{"mlx5_2:1", "mlx5_3:1"}) {
		t.Fatalf("roce node = %#v", got)
	}

	// Overrides that span both kinds leave the link layer unknown.
	out = Resolve(Inputs{Nodes: nodes[:1], Networks: []Network{rdma}, Attachments: []Attachment{
		{ID: 1, NetworkID: 1, NodeID: 1, HCAs: []string{"mlx5_0", "mlx5_bond_0"}},
	}})
	if got := find(t, out, 1, RoleCompute); got.LinkLayer != "" {
		t.Fatalf("mixed override link layer = %q", got.LinkLayer)
	}
}

func TestManagementCarriesRolesNothingElseIsAttachedFor(t *testing.T) {
	nodes := []Node{
		{ID: 1, Labels: map[string]string{"pool": "ib"}, Inventory: managedInventory("192.0.2.10", "eth0")},
		{ID: 2, Inventory: managedInventory("192.0.2.11", "eth0"), RolesOff: rolesOff(RoleServing)},
		{ID: 3, Inventory: h20Inventory()}, // an older agent: no management address
	}
	ib := network(7, "ib", TypeHostEthernet, "10.96.0.0/13", "", RoleCompute)
	out := Resolve(Inputs{Nodes: nodes, Networks: []Network{ib}, Attachments: []Attachment{{ID: 1, NetworkID: 7, LabelKey: "pool", LabelValue: "ib"}}})

	// An attached network wins; the management network fills the other roles.
	if r := find(t, out, 1, RoleCompute); r.IsManagement() || r.Network.ID != 7 || r.IP != "10.96.0.5" {
		t.Fatalf("compute = %+v", r)
	}
	for _, role := range []string{RoleStorageSync, RoleServing} {
		r := find(t, out, 1, role)
		if !r.IsManagement() || r.Network.Name != ManagementName || r.Network.ID != 0 || r.IP != "192.0.2.10" || r.Interface != "eth0" {
			t.Fatalf("%s = %+v", role, r)
		}
	}
	// A role turned off stays off; the others remain.
	absent(t, out, 2, RoleServing)
	if r := find(t, out, 2, RoleCompute); !r.IsManagement() || r.IP != "192.0.2.11" {
		t.Fatalf("node 2 compute = %+v", r)
	}
	for _, role := range Roles {
		absent(t, out, 3, role)
	}
}

func TestResolutionForAndOf(t *testing.T) {
	rs := Resolution{
		{NodeID: 1, Role: RoleCompute, Network: Network{Name: "p"}},
		{NodeID: 1, Role: RoleServing, Network: Network{Name: "s"}},
		{NodeID: 2, Role: RoleCompute, Network: Network{Name: "p2"}},
	}
	if r, ok := rs.For(1, RoleCompute); !ok || r.Network.Name != "p" {
		t.Fatalf("For(1, compute) = %+v, %v", r, ok)
	}
	if _, ok := rs.For(1, RoleStorageSync); ok {
		t.Fatal("For found a role that was never resolved")
	}
	if _, ok := rs.For(99, RoleCompute); ok {
		t.Fatal("For found a node that was never resolved")
	}
	of := rs.Of(1)
	if len(of) != 2 || of[0].Role != RoleCompute || of[1].Role != RoleServing {
		t.Fatalf("Of(1) = %#v", of)
	}
	if of := rs.Of(3); of != nil {
		t.Fatalf("Of(missing) = %#v, want nil", of)
	}
}

// TestManagementReturnsFreshRolesSlice guards against Management() sharing its
// Roles backing array across calls (it must be safe for a caller to mutate).
func TestManagementReturnsFreshRolesSlice(t *testing.T) {
	m1 := Management()
	m1.Roles[0] = "hacked"
	m2 := Management()
	if m2.Roles[0] == "hacked" {
		t.Fatal("Management shares its Roles slice across calls")
	}
	if !reflect.DeepEqual(m2.Roles, Roles) {
		t.Fatalf("Management().Roles = %v, want %v", m2.Roles, Roles)
	}
}

func TestManagementEndpointFalseWithoutManagementAddress(t *testing.T) {
	if _, ok := ManagementEndpoint(Node{ID: 1}); ok {
		t.Fatal("no inventory but ManagementEndpoint reported one")
	}
	if _, ok := ManagementEndpoint(Node{ID: 1, Inventory: &proto.NetInventory{}}); ok {
		t.Fatal("no management address but ManagementEndpoint reported one")
	}
	if _, ok := ManagementEndpoint(Node{ID: 1, Inventory: &proto.NetInventory{Management: &proto.ManagementAddress{IP: ""}}}); ok {
		t.Fatal("empty management ip but ManagementEndpoint reported one")
	}
	addr := &proto.ManagementAddress{Interface: "eth0", IP: "10.0.0.1"}
	r, ok := ManagementEndpoint(Node{ID: 1, Inventory: &proto.NetInventory{Management: addr}})
	if !ok || r.Interface != "eth0" || r.IP != "10.0.0.1" || r.Role != RoleCompute || !r.IsManagement() {
		t.Fatalf("ManagementEndpoint = %+v, %v", r, ok)
	}
}
