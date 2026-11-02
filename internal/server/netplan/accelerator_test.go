package netplan

import (
	"reflect"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The two 910B4 hosts as they reported on 2026-09-30. huawei31 owns cards 3
// and 4 only (the others are passed through to VMs); huawei32 owns all eight,
// card 6's port being down. The cards' addresses share one /16 that no host
// interface is in.
const huawei31Inventory = `{"interfaces":[
 {"name":"br_ens3_36","state":"up","mtu":1500,"speed_mbps":25000,"addrs":["172.30.8.31/16"]},
 {"name":"enp61s0f0","state":"up","mtu":9000,"speed_mbps":25000,"addrs":["100.64.29.209/30"]}],
 "rdma_ports":[
 {"device":"rocep61s0f0","port":1,"state":"ACTIVE","link_layer":"Ethernet","rate_gbps":25,"netdev":"enp61s0f0"}],
 "accelerator_ports":[
 {"name":"npu3","index":3,"ip":"172.20.9.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu4","index":4,"ip":"172.20.9.60","netmask":"255.255.0.0","state":"UP","speed_gbps":200}],
 "management":{"interface":"br_ens3_36","ip":"172.30.8.31"}}`

const huawei32Inventory = `{"interfaces":[
 {"name":"br_ens3_36","state":"up","mtu":1500,"speed_mbps":25000,"addrs":["172.30.8.32/16"]},
 {"name":"enp61s0f0","state":"up","mtu":9000,"speed_mbps":25000,"addrs":["100.64.29.210/30"]}],
 "rdma_ports":[
 {"device":"rocep61s0f0","port":1,"state":"ACTIVE","link_layer":"Ethernet","rate_gbps":25,"netdev":"enp61s0f0"}],
 "accelerator_ports":[
 {"name":"npu0","index":0,"ip":"172.20.9.79","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu1","index":1,"ip":"172.20.9.80","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu2","index":2,"ip":"172.20.9.81","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu3","index":3,"ip":"172.20.9.82","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu4","index":4,"ip":"172.20.9.83","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu5","index":5,"ip":"172.20.9.84","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
 {"name":"npu6","index":6,"ip":"172.20.9.85","netmask":"255.255.0.0","state":"DOWN"},
 {"name":"npu7","index":7,"ip":"172.20.9.86","netmask":"255.255.0.0","state":"UP","speed_gbps":200}],
 "management":{"interface":"br_ens3_36","ip":"172.30.8.32"}}`

func acceleratorResolve(t *testing.T, network Network, attachments ...Attachment) (Resolved, Resolved) {
	t.Helper()
	network.ID, network.Name, network.Type, network.Roles = 7, "npu-net", TypeAcceleratorRDMA, []string{RoleCompute}
	nodes := []Node{
		{ID: 1, Name: "huawei31", Inventory: decodeInventory(t, huawei31Inventory)},
		{ID: 2, Name: "huawei32", Inventory: decodeInventory(t, huawei32Inventory)},
	}
	if len(attachments) == 0 {
		attachments = []Attachment{{NodeID: 1}, {NodeID: 2}}
	}
	for i := range attachments {
		attachments[i].ID, attachments[i].NetworkID = uint(i+1), 7
	}
	out := Resolve(Inputs{Nodes: nodes, Networks: []Network{network}, Attachments: attachments})
	a, _ := out.For(1, RoleCompute)
	b, _ := out.For(2, RoleCompute)
	return a, b
}

func warningCodes(r Resolved) []string {
	codes := []string{}
	for _, w := range r.Warnings {
		codes = append(codes, w.Code)
	}
	return codes
}

// With nothing configured the rails are the cards whose ports work, in card
// order, and the bootstrap is the management address: no host interface can
// hold a card's address.
func TestAcceleratorNetworkInfersCardsAndAHostBootstrap(t *testing.T) {
	a, b := acceleratorResolve(t, Network{})
	if want := []string{"npu3", "npu4"}; !reflect.DeepEqual(a.HCAs, want) {
		t.Fatalf("huawei31 rails = %v, want %v", a.HCAs, want)
	}
	if want := []string{"npu0", "npu1", "npu2", "npu3", "npu4", "npu5", "npu7"}; !reflect.DeepEqual(b.HCAs, want) {
		t.Fatalf("huawei32 rails = %v, want %v (the card whose port is down left out)", b.HCAs, want)
	}
	if a.Interface != "br_ens3_36" || a.IP != "172.30.8.31" || b.IP != "172.30.8.32" {
		t.Fatalf("bootstrap = %s %s / %s, want the management addresses", a.Interface, a.IP, b.IP)
	}
	if a.LinkLayer != LinkLayerRoCE || b.LinkLayer != LinkLayerRoCE || a.HCAsPinned || a.BootstrapPinned {
		t.Fatalf("huawei31 = %#v", a)
	}
	// The two nodes use different numbers of cards, which both are told.
	for _, r := range []Resolved{a, b} {
		if got := warningCodes(r); !reflect.DeepEqual(got, []string{WarningHCAMismatch}) {
			t.Fatalf("node %d warnings = %v", r.NodeID, got)
		}
	}
	if got := a.Warnings[0].Params; got["count"] != "2" || got["counts"] != "2, 7" {
		t.Fatalf("mismatch params = %v", got)
	}
}

func TestAcceleratorHCCSNetworkUsesHCCSVNICRails(t *testing.T) {
	inv := decodeInventory(t, huawei31Inventory)
	for i := range inv.AcceleratorPorts {
		inv.AcceleratorPorts[i].HCCSIP = "192.9.2." + strconv.Itoa(199-i*2)
		inv.AcceleratorPorts[i].HCCSNetmask = "255.0.0.0"
		inv.AcceleratorPorts[i].HCCSState = "UP"
	}
	network := Network{ID: 7, Name: "hccs", Type: TypeAcceleratorHCCS, Roles: []string{RoleCompute}}
	out := Resolve(Inputs{
		Nodes:       []Node{{ID: 1, Name: "a3", Inventory: inv}},
		Networks:    []Network{network},
		Attachments: []Attachment{{ID: 1, NetworkID: 7, NodeID: 1}},
	})
	r, ok := out.For(1, RoleCompute)
	if !ok || !reflect.DeepEqual(r.HCAs, []string{"npu3", "npu4"}) {
		t.Fatalf("resolved = %#v, ok %v", r, ok)
	}
	if r.LinkLayer != LinkLayerHCCS || r.Interface != "br_ens3_36" || r.IP == "" {
		t.Fatalf("resolved network = %#v", r)
	}
	rails := Rails(inv, TypeAcceleratorHCCS, r.HCAs)
	if rails[0].IP() != "192.9.2.199" || rails[0].LinkLayer != LinkLayerHCCS {
		t.Fatalf("HCCS rails = %#v", rails)
	}
}

func TestAcceleratorNetworkBootstrapFollowsTheCIDROrAPin(t *testing.T) {
	// A CIDR picks the bootstrap among the host's interfaces.
	a, b := acceleratorResolve(t, Network{CIDR: "100.64.29.208/29"})
	if a.Interface != "enp61s0f0" || a.IP != "100.64.29.209" || b.IP != "100.64.29.210" || a.BootstrapPinned {
		t.Fatalf("bootstrap = %s %s / %s", a.Interface, a.IP, b.IP)
	}
	// The cards' own subnet matches no host interface: nothing can bind it.
	a, _ = acceleratorResolve(t, Network{CIDR: "172.20.0.0/16"})
	if a.IP != "" || !reflect.DeepEqual(warningCodes(a), []string{WarningBootstrapMissing, WarningHCAMismatch}) {
		t.Fatalf("bootstrap in the cards' subnet = %q, warnings %v", a.IP, warningCodes(a))
	}
	// A node attachment pins it.
	a, _ = acceleratorResolve(t, Network{}, Attachment{NodeID: 1, Interface: "enp61s0f0"}, Attachment{NodeID: 2})
	if a.Interface != "enp61s0f0" || a.IP != "100.64.29.209" || !a.BootstrapPinned {
		t.Fatalf("pinned bootstrap = %#v", a)
	}
}

func TestAcceleratorNetworkPinsCards(t *testing.T) {
	a, b := acceleratorResolve(t, Network{},
		Attachment{NodeID: 1, HCAs: []string{"npu4", "npu9"}},
		Attachment{NodeID: 2, HCAs: []string{"npu4", "npu6"}})
	if !reflect.DeepEqual(a.HCAs, []string{"npu4", "npu9"}) || !a.HCAsPinned {
		t.Fatalf("huawei31 = %#v", a)
	}
	if got := warningCodes(a); !reflect.DeepEqual(got, []string{WarningOverrideMissing}) || a.Warnings[0].Params["value"] != "npu9" || a.Warnings[0].Params["kind"] != "hca" {
		t.Fatalf("huawei31 warnings = %#v", a.Warnings)
	}
	if got := warningCodes(b); !reflect.DeepEqual(got, []string{WarningHCAUnusable}) || b.Warnings[0].Params["value"] != "npu6" {
		t.Fatalf("huawei32 warnings = %#v", b.Warnings)
	}
	// A host RDMA device name is not a card.
	a, _ = acceleratorResolve(t, Network{}, Attachment{NodeID: 1, HCAs: []string{"rocep61s0f0"}}, Attachment{NodeID: 2})
	if got := warningCodes(a); len(got) == 0 || got[0] != WarningOverrideMissing {
		t.Fatalf("an RDMA device pinned on an accelerator network: %v", got)
	}
}

func TestAcceleratorNetworkWithoutCards(t *testing.T) {
	plain := &proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"10.0.0.5/24"}}},
		RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: "10.0.0.5"},
	}
	network := Network{ID: 7, Type: TypeAcceleratorRDMA, Roles: []string{RoleCompute}}
	out := Resolve(Inputs{Nodes: []Node{{ID: 1, Inventory: plain}}, Networks: []Network{network}, Attachments: []Attachment{{ID: 1, NetworkID: 7, NodeID: 1}}})
	r, ok := out.For(1, RoleCompute)
	if !ok || len(r.HCAs) != 0 || r.LinkLayer != "" || !reflect.DeepEqual(warningCodes(r), []string{WarningNoHCA}) {
		t.Fatalf("a node without cards: %#v", r)
	}
	// The host's own RDMA ports are not rails of this network.
	if got := AllRails(plain, TypeAcceleratorRDMA); len(got) != 0 {
		t.Fatalf("rails = %v", got)
	}
	// A node that reported nothing yet resolves empty, without warnings.
	out = Resolve(Inputs{Nodes: []Node{{ID: 1}}, Networks: []Network{network}, Attachments: []Attachment{{ID: 1, NetworkID: 7, NodeID: 1}}})
	if r, _ := out.For(1, RoleCompute); len(r.HCAs) != 0 || len(r.Warnings) != 0 {
		t.Fatalf("unreported node: %#v", r)
	}
}

func TestAcceleratorRailsDescribeTheCard(t *testing.T) {
	inv := decodeInventory(t, huawei32Inventory)
	rails := Rails(inv, TypeAcceleratorRDMA, []string{"npu3", "npu6", "npu9", "rocep61s0f0:1"})
	if got := rails[0]; got.Name != "npu3" || got.Addr != "172.20.9.82/16" || got.IP() != "172.20.9.82" || got.NetDev != "" ||
		got.LinkLayer != LinkLayerRoCE || got.State != "UP" || got.SpeedGbps != 200 || got.Gap() != "" {
		t.Fatalf("npu3 = %#v", got)
	}
	if got := rails[1]; got.State != "DOWN" || got.Gap() != RailInactive {
		t.Fatalf("npu6 = %#v gap %q", got, got.Gap())
	}
	if got := rails[2]; got.Name != "npu9" || got.Gap() != RailUnknown {
		t.Fatalf("npu9 = %#v", got)
	}
	// The host's RDMA ports are rails of a host RDMA network, not of this one.
	if got := rails[3]; got.Gap() != RailUnknown {
		t.Fatalf("rocep61s0f0:1 on an accelerator network = %#v", got)
	}
	if got := Rails(inv, TypeHostRDMA, []string{"rocep61s0f0:1", "npu3"}); got[0].NetDev != "enp61s0f0" || got[0].IP() != "100.64.29.210" || got[1].Gap() != RailUnknown {
		t.Fatalf("host RDMA rails = %#v", got)
	}
	// A card without an address cannot carry traffic; a mask that is not
	// one leaves the address without a prefix.
	odd := &proto.NetInventory{AcceleratorPorts: []proto.AcceleratorPort{
		{Name: "npu0", Index: 0, State: "UP"},
		{Name: "npu1", Index: 1, IP: "172.20.9.80", Netmask: "garbage", State: "UP"},
		{Name: "npu2", Index: 2, IP: "fd00::2", State: "UP"},
	}}
	got := AllRails(odd, TypeAcceleratorRDMA)
	if got[0].Gap() != RailNoIPv4 || got[1].Addr != "172.20.9.80" || got[1].Gap() != "" || got[2].Gap() != RailNoIPv4 {
		t.Fatalf("odd rails = %#v", got)
	}
	// Cards keep their inventory order, not a lexical one (npu10 after npu2).
	many := &proto.NetInventory{}
	for _, i := range []int{0, 2, 10} {
		many.AcceleratorPorts = append(many.AcceleratorPorts, proto.AcceleratorPort{Name: "npu" + strconv.Itoa(i), Index: i, IP: "172.20.9.1", Netmask: "255.255.0.0", State: "UP"})
	}
	if got := inferRails(many, TypeAcceleratorRDMA); !reflect.DeepEqual(got, []string{"npu0", "npu2", "npu10"}) {
		t.Fatalf("inferred = %v", got)
	}
}

func TestTypeRoles(t *testing.T) {
	if !reflect.DeepEqual(Types, []string{"host_ethernet", "host_rdma", "accelerator_rdma", "accelerator_hccs"}) {
		t.Fatalf("types = %v", Types)
	}
	for typ, want := range map[string][]string{
		TypeHostEthernet:    {RoleStorageSync, RoleCompute, RoleServing},
		TypeHostRDMA:        {RoleStorageSync, RoleCompute, RoleServing},
		TypeAcceleratorRDMA: {RoleCompute},
		TypeAcceleratorHCCS: {RoleCompute},
	} {
		if got := TypeRoles(typ); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s roles = %v, want %v", typ, got, want)
		}
	}
	if RoleAllowed(TypeAcceleratorRDMA, RoleServing) || RoleAllowed(TypeAcceleratorRDMA, RoleStorageSync) || !RoleAllowed(TypeAcceleratorRDMA, RoleCompute) {
		t.Fatal("an accelerator network carries compute traffic only")
	}
	// The dictionary handed out is a copy.
	TypeRoles(TypeHostRDMA)[0] = "changed"
	if Roles[0] != RoleStorageSync {
		t.Fatal("TypeRoles exposed the Roles slice")
	}
	if UsesRails(TypeHostEthernet) || !UsesRails(TypeHostRDMA) || !UsesRails(TypeAcceleratorRDMA) || UsesRails("") {
		t.Fatal("UsesRails")
	}
	if Management().Type != TypeHostEthernet {
		t.Fatalf("management type = %q", Management().Type)
	}
}
