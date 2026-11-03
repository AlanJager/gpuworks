package netplan

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ascendInventory is ascend-31 as it reported on 2026-09-28: four hns RoCE
// ports, three on 25G interfaces with a /30 each, one on a 1G interface
// (enp189s0f0) whose address was removed, which also reports a bogus 2.5G
// port rate.
const ascendInventory = `{"interfaces":[
 {"name":"br_ens3_36","state":"up","mtu":1500,"speed_mbps":25000,"addrs":["198.51.100.31/16"]},
 {"name":"enp189s0f0","state":"up","mtu":1500,"speed_mbps":1000},
 {"name":"enp189s0f2","state":"down","mtu":1500},
 {"name":"enp61s0f0","state":"up","mtu":9000,"speed_mbps":25000,"addrs":["100.64.29.209/30"]},
 {"name":"enp61s0f1","state":"up","mtu":9000,"speed_mbps":25000,"addrs":["100.64.29.217/30"]},
 {"name":"enp61s0f2","state":"up","mtu":9000,"speed_mbps":25000,"addrs":["100.64.29.213/30"]}],
 "rdma_ports":[
 {"device":"rocep189s0f0","port":1,"state":"ACTIVE","link_layer":"Ethernet","rate_gbps":2.5,"netdev":"enp189s0f0"},
 {"device":"rocep189s0f2","port":1,"state":"DOWN","link_layer":"Ethernet","rate_gbps":2.5,"netdev":"enp189s0f2"},
 {"device":"rocep61s0f0","port":1,"state":"ACTIVE","link_layer":"Ethernet","rate_gbps":25,"netdev":"enp61s0f0"},
 {"device":"rocep61s0f2","port":1,"state":"ACTIVE","link_layer":"Ethernet","rate_gbps":25,"netdev":"enp61s0f2"}],
 "tools":["ib_write_bw"],"management":{"interface":"br_ens3_36","ip":"198.51.100.31"}}`

// h200Inventory is a Nebius 8x400G InfiniBand host: the ib* interfaces
// carry no address and only eth0 (the management address) has one.
const h200Inventory = `{"interfaces":[
 {"name":"eth0","state":"up","addrs":["10.96.0.56/32"]},
 {"name":"ibp140s0","state":"down"},{"name":"ibp144s0","state":"down"},{"name":"ibp148s0","state":"down"},{"name":"ibp152s0","state":"down"},
 {"name":"ibp170s0","state":"down"},{"name":"ibp174s0","state":"down"},{"name":"ibp178s0","state":"down"},{"name":"ibp182s0","state":"down"}],
 "rdma_ports":[
 {"device":"mlx5_0","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp170s0"},
 {"device":"mlx5_1","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp174s0"},
 {"device":"mlx5_2","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp178s0"},
 {"device":"mlx5_3","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp182s0"},
 {"device":"mlx5_4","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp140s0"},
 {"device":"mlx5_5","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp144s0"},
 {"device":"mlx5_6","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp148s0"},
 {"device":"mlx5_7","port":1,"state":"ACTIVE","link_layer":"InfiniBand","rate_gbps":400,"netdev":"ibp152s0"}],
 "tools":["ib_write_bw"],"management":{"interface":"eth0","ip":"10.96.0.56"}}`

func decodeInventory(t *testing.T, raw string) *proto.NetInventory {
	t.Helper()
	var inv proto.NetInventory
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		t.Fatal(err)
	}
	return &inv
}

func resolveOne(inv *proto.NetInventory, n Network, a Attachment) Resolved {
	n.ID, n.Type, n.Roles = 5, TypeHostRDMA, []string{RoleCompute}
	a.ID, a.NetworkID, a.NodeID = 1, 5, 1
	node := Node{ID: 1, Name: "n1", Inventory: inv}
	for _, r := range Resolve(Inputs{Nodes: []Node{node}, Networks: []Network{n}, Attachments: []Attachment{a}}) {
		if r.Role == RoleCompute {
			return r
		}
	}
	return Resolved{}
}

// The two lab fabrics an operator configured by hand on 2026-09-28 resolve
// to the same rails and bootstrap with nothing configured.
func TestRDMAInfersLabFabrics(t *testing.T) {
	ascend := resolveOne(decodeInventory(t, ascendInventory), Network{}, Attachment{})
	if want := []string{"rocep61s0f0:1", "rocep61s0f2:1"}; !reflect.DeepEqual(ascend.HCAs, want) {
		t.Fatalf("ascend rails = %v, want %v", ascend.HCAs, want)
	}
	if ascend.Interface != "enp61s0f0" || ascend.IP != "100.64.29.209" || ascend.LinkLayer != LinkLayerRoCE || len(ascend.Warnings) != 0 {
		t.Fatalf("ascend = %#v", ascend)
	}
	if ascend.HCAsPinned || ascend.BootstrapPinned {
		t.Fatalf("inferred endpoints marked pinned: %#v", ascend)
	}

	h200 := resolveOne(decodeInventory(t, h200Inventory), Network{}, Attachment{})
	if len(h200.HCAs) != 8 || h200.HCAs[0] != "mlx5_0:1" || h200.HCAs[7] != "mlx5_7:1" {
		t.Fatalf("h200 rails = %v", h200.HCAs)
	}
	// InfiniBand without IPoIB: the handshake uses the management address.
	if h200.Interface != "eth0" || h200.IP != "10.96.0.56" || h200.LinkLayer != LinkLayerInfiniBand || len(h200.Warnings) != 0 {
		t.Fatalf("h200 = %#v", h200)
	}
}

// With its address back, the 1G port is usable but slower than the rails,
// so it still stays out.
func TestRDMAInferenceKeepsOnlyTheFastestRails(t *testing.T) {
	inv := decodeInventory(t, ascendInventory)
	inv.Interfaces[1].Addrs = []string{"100.64.0.1/30"}
	got := resolveOne(inv, Network{}, Attachment{})
	if want := []string{"rocep61s0f0:1", "rocep61s0f2:1"}; !reflect.DeepEqual(got.HCAs, want) {
		t.Fatalf("rails = %v, want %v", got.HCAs, want)
	}
}

func TestRDMABootstrapCIDRAndOverride(t *testing.T) {
	inv := decodeInventory(t, ascendInventory)
	// A bootstrap CIDR picks the address inside it, even off the rails.
	got := resolveOne(inv, Network{CIDR: "198.51.100.0/24"}, Attachment{})
	if got.Interface != "br_ens3_36" || got.IP != "198.51.100.31" || len(got.Warnings) != 0 {
		t.Fatalf("cidr = %#v", got)
	}
	// A CIDR nothing is in leaves no bootstrap and says so; no fallback.
	got = resolveOne(inv, Network{CIDR: "10.0.0.0/8"}, Attachment{})
	if got.IP != "" || !reflect.DeepEqual(got.Warnings, []Warning{{Code: WarningBootstrapMissing}}) {
		t.Fatalf("unmatched cidr = %#v", got)
	}
	// A node's pinned bootstrap beats the CIDR.
	got = resolveOne(inv, Network{CIDR: "198.51.100.0/24"}, Attachment{Interface: "enp61s0f1"})
	if got.Interface != "enp61s0f1" || got.IP != "100.64.29.217" || !got.BootstrapPinned || got.HCAsPinned {
		t.Fatalf("override = %#v", got)
	}
}

func TestRDMAPinnedRails(t *testing.T) {
	inv := decodeInventory(t, ascendInventory)
	got := resolveOne(inv, Network{}, Attachment{HCAs: []string{"rocep61s0f2", "rocep189s0f0:1", "rocep189s0f2"}})
	if want := []string{"rocep61s0f2:1", "rocep189s0f0:1", "rocep189s0f2:1"}; !reflect.DeepEqual(got.HCAs, want) || !got.HCAsPinned {
		t.Fatalf("pinned = %#v", got)
	}
	// The bootstrap follows the first pinned rail; the addressless and down
	// ports are flagged.
	want := []Warning{{Code: WarningHCAUnusable, Params: map[string]string{"value": "rocep189s0f0:1, rocep189s0f2:1"}}}
	if got.Interface != "enp61s0f2" || got.IP != "100.64.29.213" || !reflect.DeepEqual(got.Warnings, want) {
		t.Fatalf("pinned endpoints = %#v", got)
	}
}

func TestRDMAWarnsWhenNothingIsUsable(t *testing.T) {
	inv := decodeInventory(t, ascendInventory)
	for i := range inv.Interfaces {
		inv.Interfaces[i].Addrs = nil
	}
	got := resolveOne(inv, Network{}, Attachment{})
	want := []Warning{{Code: WarningNoHCA}, {Code: WarningBootstrapMissing}}
	if len(got.HCAs) != 0 || got.IP != "" || !reflect.DeepEqual(got.Warnings, want) {
		t.Fatalf("got %#v", got)
	}
	// A node that never reported an inventory is not warned about yet.
	if got := resolveOne(nil, Network{}, Attachment{}); len(got.Warnings) != 0 || len(got.HCAs) != 0 {
		t.Fatalf("no inventory = %#v", got)
	}
}

func TestRDMARailCountMismatch(t *testing.T) {
	ascend := decodeInventory(t, ascendInventory)
	n := Network{ID: 5, Name: "f", Type: TypeHostRDMA, Roles: []string{RoleCompute}}
	nodes := []Node{{ID: 1, Inventory: ascend}, {ID: 2, Inventory: ascend}}
	out := Resolve(Inputs{Nodes: nodes, Networks: []Network{n}, Attachments: []Attachment{
		{ID: 1, NetworkID: 5, NodeID: 1},
		{ID: 2, NetworkID: 5, NodeID: 2, HCAs: []string{"rocep61s0f0"}},
	}})
	want := func(count string) []Warning {
		return []Warning{{Code: WarningHCAMismatch, Params: map[string]string{"count": count, "counts": "1, 2"}}}
	}
	if got := find(t, out, 1, RoleCompute); !reflect.DeepEqual(got.Warnings, want("2")) {
		t.Fatalf("node 1 warnings = %#v", got.Warnings)
	}
	if got := find(t, out, 2, RoleCompute); !reflect.DeepEqual(got.Warnings, want("1")) {
		t.Fatalf("node 2 warnings = %#v", got.Warnings)
	}
}

func TestRailsDescribePorts(t *testing.T) {
	inv := decodeInventory(t, ascendInventory)
	got := Rails(inv, TypeHostRDMA, []string{"rocep61s0f0:1", "rocep189s0f0:1", "mlx5_9:1"})
	want := []Rail{
		{Name: "rocep61s0f0:1", NetDev: "enp61s0f0", Addr: "100.64.29.209/30", LinkLayer: LinkLayerRoCE, State: "ACTIVE", SpeedGbps: 25},
		{Name: "rocep189s0f0:1", NetDev: "enp189s0f0", LinkLayer: LinkLayerRoCE, State: "ACTIVE", SpeedGbps: 1},
		{Name: "mlx5_9:1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rails = %#v", got)
	}
	if got[0].IP() != "100.64.29.209" || got[0].Gap() != "" || got[1].Gap() != RailNoIPv4 || got[2].Gap() != RailUnknown {
		t.Fatalf("ip/gaps = %q %q %q %q", got[0].IP(), got[0].Gap(), got[1].Gap(), got[2].Gap())
	}
	if gap := AllRails(inv, TypeHostRDMA)[1].Gap(); gap != RailInactive {
		t.Fatalf("down port gap = %q", gap)
	}
	if len(AllRails(nil, TypeHostRDMA)) != 0 || len(Rails(nil, TypeHostRDMA, []string{"x:1"})) != 1 {
		t.Fatal("nil inventory")
	}
}
