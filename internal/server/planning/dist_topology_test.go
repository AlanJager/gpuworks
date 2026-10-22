package planning

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Every rank of a unit takes its place from RankTopology: rank 0 leads, the
// followers are headless, and each advertises its address on the network the
// group shares. The controller and the previews read the same function.
func TestRankTopologyPlacesEveryRankOfAGeneration(t *testing.T) {
	first := &store.Node{ID: 11, UUID: "node-a", Hostname: "node-a.example"}
	second := &store.Node{ID: 12, UUID: "node-b", Hostname: "node-b.example"}
	onNetwork := func(ip string) *netplan.Resolved {
		return &netplan.Resolved{Network: netplan.Management(), Source: netplan.SourceManagement, Interface: "eth0", IP: ip}
	}
	generation := GroupRank{Ranks: 2, Group: "deployment-7", Attempt: 3}

	leader, err := RankTopology(first, onNetwork("10.0.0.5"), generation)
	if err != nil {
		t.Fatal(err)
	}
	onManagement := deploy.ComputeNetwork{Type: netplan.TypeHostEthernet, Interface: "eth0"}
	want := DistTopology{NNodes: 2, NodeRank: 0, Headless: false, Role: "leader", Group: "deployment-7", Attempt: 3, NodeAddress: "10.0.0.5", Network: onManagement, NetworkName: "management"}
	if !reflect.DeepEqual(leader, want) {
		t.Fatalf("leader topology = %+v, want %+v", leader, want)
	}
	generation.Rank = 1
	follower, err := RankTopology(second, onNetwork("10.0.0.6"), generation)
	if err != nil {
		t.Fatal(err)
	}
	want = DistTopology{NNodes: 2, NodeRank: 1, Headless: true, Role: "follower", Group: "deployment-7", Attempt: 3, NodeAddress: "10.0.0.6", Network: onManagement, NetworkName: "management"}
	if !reflect.DeepEqual(follower, want) {
		t.Fatalf("follower topology = %+v, want %+v", follower, want)
	}

	// A single rank with no compute network advertises the host a peer
	// would dial, read leniently: a port is stripped, the hostname serves.
	singleNode := &store.Node{ID: 11, UUID: "node-a", Hostname: "node-a.example:8000"}
	topology, err := RankTopology(singleNode, nil, GroupRank{})
	if err != nil {
		t.Fatal(err)
	}
	if topology.NNodes != 1 || topology.Headless || topology.Role != "leader" || topology.Group != "" || topology.Attempt != 0 || topology.NodeAddress != "node-a.example" {
		t.Fatalf("single-rank topology = %+v", topology)
	}
}

// A rank of a multi-node unit without an address on the network its group
// shares refuses to start: a member the collective cannot dial would hang
// every other rank with it. A single rank has nobody to be dialled by.
func TestRankTopologyRefusesAGroupRankWithoutAnAddress(t *testing.T) {
	node := &store.Node{ID: 11, UUID: "node-a"}
	if _, err := RankTopology(node, nil, GroupRank{Ranks: 2}); err == nil {
		t.Fatal("a multi-node rank started without a network")
	}
	if _, err := RankTopology(node, &netplan.Resolved{Network: netplan.Management()}, GroupRank{Ranks: 2, Rank: 1}); err == nil {
		t.Fatal("a multi-node rank started without an address on its network")
	}
	if _, err := RankTopology(nil, nil, GroupRank{}); err == nil {
		t.Fatal("a missing node was accepted")
	}
	if _, err := RankTopology(node, nil, GroupRank{}); err != nil {
		t.Fatalf("a single rank needs no compute network: %v", err)
	}
}

func TestRankTopologyUsesTheComputeNetwork(t *testing.T) {
	ib := netplan.Network{ID: 3, Name: "ib-a", Type: netplan.TypeHostRDMA}
	node := &store.Node{ID: 12, UUID: "node-b"}
	got, err := RankTopology(node, &netplan.Resolved{Network: ib, Interface: "bond0", IP: "10.20.1.12", HCAs: []string{"mlx5_0:1"}}, GroupRank{Ranks: 2, Rank: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := deploy.ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "bond0", HCAs: []string{"mlx5_0:1"}}
	if got.NodeAddress != "10.20.1.12" || got.NetworkID != 3 || !reflect.DeepEqual(got.Network, want) {
		t.Fatalf("topology = %+v", got)
	}
	// Without a compute endpoint a single rank advertises the node's
	// management address.
	node.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"management":{"interface":"eth0","ip":"10.0.0.6"}}`
	got, err = RankTopology(node, nil, GroupRank{})
	if err != nil || got.NetworkID != 0 || got.NodeAddress != "10.0.0.6" {
		t.Fatalf("single rank topology = %+v, %v", got, err)
	}
}
