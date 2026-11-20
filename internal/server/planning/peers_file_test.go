package planning

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func peersNode(t *testing.T, id uint, name string, inventory proto.NetInventory) *store.Node {
	t.Helper()
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{UUID: name + "-uuid", Name: name, Hostname: name, NetworkInventory: string(raw)}
	node.ID = id
	return node
}

// gb10Inventory is a DGX Spark as its agent reports it: two RoCE ports, each
// on its own subnet, plus the management interface.
func gb10Inventory(host int) proto.NetInventory {
	a, b, mgmt := "192.168.177."+strconv.Itoa(host), "192.168.178."+strconv.Itoa(host), "10.0.0."+strconv.Itoa(host)
	return proto.NetInventory{
		Interfaces: []proto.NetInterface{
			{Name: "enp1s0f1np1", Addrs: []string{a + "/24"}}, {Name: "enP2p1s0f1np1", Addrs: []string{b + "/24"}}, {Name: "eno1", Addrs: []string{mgmt + "/24"}},
		},
		RDMAPorts: []proto.RDMAPort{
			{Device: "rocep1s0f1", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp1s0f1np1", RoCEAddress: &proto.RoCEAddress{NetDev: "enp1s0f1np1", CIDR: a + "/24"}},
			{Device: "roceP2p1s0f1", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enP2p1s0f1np1", RoCEAddress: &proto.RoCEAddress{NetDev: "enP2p1s0f1np1", CIDR: b + "/24"}},
		},
		Management: &proto.ManagementAddress{Interface: "eno1", IP: mgmt},
	}
}

// Every rank is listed in rank order with its advertised address and the
// links of the shared compute network, each named with its interface and its
// own address, which is what a recipe writes into its fleet configuration.
func TestGroupPeersListsEveryRankWithItsLinks(t *testing.T) {
	nodes := []*store.Node{peersNode(t, 1, "gb10-1", gb10Inventory(51)), peersNode(t, 2, "gb10-2", gb10Inventory(52))}
	network := netplan.Network{ID: 7, Name: "roce", Type: netplan.TypeHostRDMA}
	group := netplan.Group{Network: network, Ranks: map[uint]netplan.Resolved{}}
	for i, node := range nodes {
		group.Ranks[node.ID] = netplan.Resolved{NodeID: node.ID, Network: network, Interface: "enp1s0f1np1", IP: "192.168.177." + strconv.Itoa(51+i),
			HCAs: []string{"rocep1s0f1:1", "roceP2p1s0f1:1"}, LinkLayer: netplan.LinkLayerRoCE}
	}
	got := GroupPeers(nodes, group)
	want := []proto.PeerRank{
		{NodeRank: 0, NodeName: "gb10-1", NodeUUID: "gb10-1-uuid", Hostname: "gb10-1", NodeAddr: "192.168.177.51", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.51"}, Interface: "enp1s0f1np1",
			HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.51"}, {Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: "192.168.178.51"}}},
		{NodeRank: 1, NodeName: "gb10-2", NodeUUID: "gb10-2-uuid", Hostname: "gb10-2", NodeAddr: "192.168.177.52", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.52"}, Interface: "enp1s0f1np1",
			HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.52"}, {Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: "192.168.178.52"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peers =\n%+v\nwant\n%+v", got, want)
	}
}

// An accelerator network's links are the cards' ports with their addresses;
// a rail the inventory does not list keeps only its name; a group over the
// management network has no links and each rank advertises its management
// host; a node with no inventory still gets its row.
func TestGroupPeersOnOtherNetworks(t *testing.T) {
	ascend := proto.NetInventory{
		AcceleratorPorts: []proto.AcceleratorPort{{Name: "npu0", Index: 0, IP: "29.0.0.1", Netmask: "255.255.255.0", State: "UP"}},
		Management:       &proto.ManagementAddress{Interface: "eth0", IP: "10.1.0.1"},
	}
	cards := netplan.Network{ID: 3, Type: netplan.TypeAcceleratorRDMA}
	nodes := []*store.Node{peersNode(t, 1, "a3-1", ascend), {UUID: "bare-uuid", Name: "bare", Hostname: "bare.local"}}
	nodes[1].ID = 2
	group := netplan.Group{Network: cards, Ranks: map[uint]netplan.Resolved{
		1: {NodeID: 1, Network: cards, Interface: "eth0", IP: "10.1.0.1", HCAs: []string{"npu0", "npu1"}, LinkLayer: netplan.LinkLayerRoCE},
		2: {NodeID: 2, Network: cards, IP: "10.1.0.2", HCAs: []string{"npu0"}},
	}}
	got := GroupPeers(nodes, group)
	if len(got) != 2 {
		t.Fatalf("peers = %+v", got)
	}
	if want := []proto.PeerHCA{{Name: "npu0", IP: "29.0.0.1"}, {Name: "npu1"}}; !reflect.DeepEqual(got[0].HCAs, want) {
		t.Fatalf("card links = %+v, want %+v", got[0].HCAs, want)
	}
	if got[1].NodeAddr != "10.1.0.2" || got[1].Management != nil || !reflect.DeepEqual(got[1].HCAs, []proto.PeerHCA{{Name: "npu0"}}) {
		t.Fatalf("uninventoried rank = %+v", got[1])
	}

	management := netplan.Group{Ranks: map[uint]netplan.Resolved{}}
	plain := []*store.Node{peersNode(t, 1, "eth-1", gb10Inventory(61)), peersNode(t, 2, "eth-2", gb10Inventory(62))}
	got = GroupPeers(plain, management)
	if got[0].NodeAddr != "10.0.0.61" || got[1].NodeAddr != "10.0.0.62" || got[0].HCAs != nil {
		t.Fatalf("management group peers = %+v", got)
	}
}

func TestGroupPeersOfASingleRankIsNil(t *testing.T) {
	if got := GroupPeers([]*store.Node{peersNode(t, 1, "solo", gb10Inventory(9))}, netplan.Group{}); got != nil {
		t.Fatalf("single rank peers = %+v", got)
	}
}

func TestPeersFileSupportedNeedsTheCapability(t *testing.T) {
	if PeersFileSupported(nil) || PeersFileSupported([]string{proto.CapLaunchNameVerbatim}) || !PeersFileSupported([]string{proto.CapPeersFile}) {
		t.Fatal("PeersFileSupported does not follow proto.CapPeersFile")
	}
}
