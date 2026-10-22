package planning

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DistTopology is one rank's place in a distributed deployment: how many nodes
// the group has, which rank this node is, whether it is a headless follower,
// the host it advertises to the collective, and the group identity every rank
// shares. The api's preview and the controller's scheduler both need it, and it
// is a function of the stored node list rather than of either caller.
type DistTopology struct {
	NNodes      int
	NodeRank    int
	Headless    bool
	Role        string
	Group       string
	Attempt     int
	NodeAddress string
	// Network is the rank's compute network; zero when the node has none.
	Network deploy.ComputeNetwork
	// NetworkID identifies that network so every rank can be checked to share
	// it; 0 with a NetworkName is the management network.
	NetworkID   uint
	NetworkName string
}

// RoutableHost is the host a peer would dial for this node: its management
// network address when its agent reported one, else its hostname, and nothing
// when neither can be read as an endpoint host.
func RoutableHost(node *store.Node) string {
	if node == nil {
		return ""
	}
	addr, _ := network.ManagementAddress(*node)
	address := addr.IP
	if address == "" {
		address = strings.TrimSpace(node.Hostname)
	}
	host, err := nodeaddr.RoutableEndpointHost(address)
	if err != nil {
		return ""
	}
	return host
}

// GroupRank is a rank's place in its unit's current generation: the unit's
// rank count, this rank, and the group identity and attempt its ranks share.
// The zero value is a single-rank unit.
type GroupRank struct {
	Ranks   int
	Rank    int
	Group   string
	Attempt int
	// Master is the address every rank of the generation rendezvous at: its
	// leader's. Empty keeps the deployment's.
	Master string
}

// RankTopology is one rank's place: rank 0 leads and serves, the rest are
// headless followers. A rank advertises its address on the network its group
// shares (network.Service.ComputeGroup); compute is that endpoint for
// this node, nil when there is none. A single rank without one advertises
// its routable host. A rank of a multi-node unit without one is refused: a
// rank the group cannot reach would join a collective that never completes,
// which is worse than not starting.
func RankTopology(node *store.Node, compute *netplan.Resolved, rank GroupRank) (DistTopology, error) {
	if node == nil {
		return DistTopology{}, fmt.Errorf("rank topology requires a node")
	}
	topology := DistTopology{NNodes: max(rank.Ranks, 1), NodeRank: rank.Rank, Group: rank.Group, Attempt: rank.Attempt, Role: "leader", NodeAddress: RoutableHost(node)}
	if topology.NodeRank > 0 {
		topology.Headless, topology.Role = true, "follower"
	}
	if compute != nil {
		topology.NetworkID, topology.NetworkName, topology.Network = rankNetwork(*compute)
		if compute.IP != "" {
			topology.NodeAddress = compute.IP
		}
	}
	if topology.NNodes > 1 && (compute == nil || compute.IP == "") {
		return DistTopology{}, fmt.Errorf("node %s has no address on the network the group shares", node.DisplayName())
	}
	return topology, nil
}

// PreviewRankTopology is one rank of a multi-node group as the deploy form
// previews it, before the deployment row exists: rank 0 leads, the rest are
// headless followers, and each advertises its address on the network the
// group shares.
func PreviewRankTopology(nnodes, rank int, compute netplan.Resolved) DistTopology {
	topology := DistTopology{NNodes: nnodes, NodeRank: rank, Headless: rank > 0, Role: "leader", NodeAddress: compute.IP}
	if topology.Headless {
		topology.Role = "follower"
	}
	topology.NetworkID, topology.NetworkName, topology.Network = rankNetwork(compute)
	return topology
}

// PeerHost validates a host another node would dial, such as a stated
// master_addr or a probe target: an IP or name without a port that is not
// loopback or unspecified.
func PeerHost(raw string) (string, error) {
	return nodeaddr.RoutableHost(strings.TrimSpace(raw))
}
