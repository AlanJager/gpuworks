package planning

import (
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// GroupPeers is every rank of a multi-node group as its peers file lists it
// (proto.PeersFile.Ranks): rank i is nodes[i], at the address it advertises
// to the group with the rails of the compute network the group shares on
// that node. The address is rankAddress, the one RankTopology gives the rank
// as NODE_ADDR. It reads only the group's own nodes and the network the
// caller already resolved for them, so it costs one inventory decode per rank
// of one generation.
//
// A single-rank group has no peers file and gets nil.
func GroupPeers(nodes []*store.Node, group netplan.Group) []proto.PeerRank {
	if len(nodes) <= 1 {
		return nil
	}
	out := make([]proto.PeerRank, 0, len(nodes))
	for rank, node := range nodes {
		if node == nil {
			return nil
		}
		var compute *netplan.Resolved
		if resolved, ok := group.Ranks[node.ID]; ok {
			compute = &resolved
		}
		peer := proto.PeerRank{NodeRank: rank, NodeUUID: node.UUID, NodeName: node.DisplayName(), Hostname: strings.TrimSpace(node.Hostname), NodeAddr: rankAddress(node, compute)}
		if management, ok := network.ManagementAddress(*node); ok {
			peer.Management = &management
		}
		if compute != nil {
			peer.Interface = compute.Interface
			peer.HCAs = peerHCAs(network.Inventory(*node), *compute)
		}
		out = append(out, peer)
	}
	return out
}

// rankAddress is the address a rank advertises to its group (NODE_ADDR): its
// address on the group's compute network, else its routable host.
func rankAddress(node *store.Node, compute *netplan.Resolved) string {
	if compute != nil && compute.IP != "" {
		return compute.IP
	}
	return RoutableHost(node)
}

// PeersFileSupported reports whether a node's agent writes a launch's peers
// file (proto.CapPeersFile). caps is what the agent advertised, nil when it
// is offline or unknown, which counts as an agent that does not: a launch
// that names a file the agent never writes fails later and less clearly than
// one that names none.
func PeersFileSupported(caps []string) bool {
	return slices.Contains(caps, proto.CapPeersFile)
}

// peerHCAs names each rail of a rank's compute network with what its node
// reported about it, as the network API describes rails (netplan.Rails): an
// RDMA port's interface and address (the RoCE address verified against its
// GID table, else its interface's), or an accelerator card port's address. A
// rail the inventory does not list keeps only its name.
func peerHCAs(inventory *proto.NetInventory, resolved netplan.Resolved) []proto.PeerHCA {
	if len(resolved.HCAs) == 0 {
		return nil
	}
	accelerator := netplan.IsAccelerator(resolved.Network.Type)
	rails := netplan.Rails(inventory, resolved.Network.Type, resolved.HCAs)
	out := make([]proto.PeerHCA, 0, len(rails))
	for _, rail := range rails {
		hca := proto.PeerHCA{Name: rail.Name, NetDev: rail.NetDev, IP: rail.IP()}
		if !accelerator {
			device, port, found := strings.Cut(rail.Name, ":")
			hca.Device = device
			if number, err := strconv.Atoi(port); found && err == nil {
				hca.Port = number
			}
		}
		out = append(out, hca)
	}
	return out
}
