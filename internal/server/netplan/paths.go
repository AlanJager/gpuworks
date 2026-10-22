package netplan

import "errors"

// ErrNoStoragePath means two nodes share no network that may carry storage
// sync: the target then downloads from the image or model source instead.
var ErrNoStoragePath = errors.New("the nodes share no storage-sync network")

// StoragePath is the network a direct copy between two nodes uses and each
// end's address on it.
type StoragePath struct {
	Network  string // network name; ManagementName for the management network
	SourceIP string
	TargetIP string
}

// StoragePathBetween picks the network a copy from source to target uses: the
// storage-sync network both nodes resolved, else the management network when
// both let it carry storage sync. resolved must cover both nodes.
func StoragePathBetween(source, target Node, resolved Resolution) (StoragePath, error) {
	rs, okS := resolved.For(source.ID, RoleStorageSync)
	rt, okT := resolved.For(target.ID, RoleStorageSync)
	if okS && okT && !rs.IsManagement() && !rt.IsManagement() && rs.Network.ID == rt.Network.ID && rs.IP != "" && rt.IP != "" {
		return StoragePath{Network: rs.Network.Name, SourceIP: rs.IP, TargetIP: rt.IP}, nil
	}
	ms, okS := managementFor(source, RoleStorageSync)
	mt, okT := managementFor(target, RoleStorageSync)
	if okS && okT {
		return StoragePath{Network: ManagementName, SourceIP: ms.IP, TargetIP: mt.IP}, nil
	}
	return StoragePath{}, ErrNoStoragePath
}

// CanServe reports whether a node may host an inference service: its
// management network may carry serving traffic over the agent tunnel (which
// needs no address), or it has a serving network of its own. resolved must
// cover the node.
func CanServe(node Node, resolved Resolution) bool {
	if !node.RolesOff[RoleServing] {
		return true
	}
	r, ok := resolved.For(node.ID, RoleServing)
	return ok && !r.IsManagement()
}

// ServingIP is where a node runs its serving forwarder: its address on a
// serving network of its own. Serving over the management network rides the
// agent tunnel and needs none, so that is "".
func ServingIP(nodeID uint, resolved Resolution) string {
	if r, ok := resolved.For(nodeID, RoleServing); ok && !r.IsManagement() {
		return r.IP
	}
	return ""
}

// ServingIPs is ServingIP for every node resolved covers, read in one pass:
// asking ServingIP node by node walks the whole resolution each time, which
// at fleet size is quadratic. Nodes serving over the tunnel are absent.
func ServingIPs(resolved Resolution) map[uint]string {
	out := map[uint]string{}
	seen := map[uint]bool{}
	for _, r := range resolved {
		if r.Role != RoleServing || seen[r.NodeID] {
			continue // For answers with a node's first serving entry
		}
		seen[r.NodeID] = true
		if !r.IsManagement() && r.IP != "" {
			out[r.NodeID] = r.IP
		}
	}
	return out
}
