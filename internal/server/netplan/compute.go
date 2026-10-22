package netplan

import "fmt"

// Why a multi-node group cannot share a compute network.
const (
	// ComputeNone: a node has no network that may carry compute traffic.
	ComputeNone = "none"
	// ComputeMixed: a node is on a compute network, another is not, and
	// the management network cannot carry the group.
	ComputeMixed = "mixed"
	// ComputeMismatch: two nodes are on different compute networks and
	// the management network cannot carry the group.
	ComputeMismatch = "mismatch"
)

// Group is the network every rank of a multi-node group uses.
type Group struct {
	Network Network
	// Ranks is each node's endpoint on Network, by node ID.
	Ranks map[uint]Resolved
	// FellBack is set when some rank has a compute network of its own but
	// the group could not share one, so it runs over the management network.
	FellBack bool
}

// GroupError says why a group has no shared compute network.
type GroupError struct {
	Reason   string // Compute*
	Node     Node
	Other    Node   // the node it disagrees with (mixed, mismatch)
	Network  string // Node's compute network (mismatch)
	Expected string // Other's compute network (mismatch)
}

func (e *GroupError) Error() string {
	switch e.Reason {
	case ComputeMixed:
		return fmt.Sprintf("node %s has a compute network but node %s has none, and the management network cannot carry compute traffic for both", e.Node.Name, e.Other.Name)
	case ComputeMismatch:
		return fmt.Sprintf("node %s is on compute network %s but node %s is on %s, and the management network cannot carry compute traffic for both", e.Node.Name, e.Network, e.Other.Name, e.Expected)
	}
	return fmt.Sprintf("node %s has no network that may carry compute traffic", e.Node.Name)
}

// ComputeGroup picks the network a multi-node group uses: the compute
// network every node is on, else the management network when every node lets
// it carry compute traffic. Nodes keep their order; the first is rank 0.
// resolved must cover every node (Resolve over the same nodes).
func ComputeGroup(nodes []Node, resolved Resolution) (Group, error) {
	own := make([]*Resolved, len(nodes))
	for i, node := range nodes {
		if r, ok := resolved.For(node.ID, RoleCompute); ok && !r.IsManagement() {
			own[i] = &r
		}
	}
	shared := len(nodes) > 0
	for _, r := range own {
		if r == nil || r.Network.ID != own[0].Network.ID {
			shared = false
		}
	}
	if shared {
		group := Group{Network: own[0].Network, Ranks: map[uint]Resolved{}}
		for i, node := range nodes {
			group.Ranks[node.ID] = *own[i]
		}
		return group, nil
	}
	group := Group{Network: Management(), Ranks: map[uint]Resolved{}}
	blocked := -1
	for i, node := range nodes {
		mgmt, ok := managementFor(node, RoleCompute)
		if !ok {
			if blocked < 0 {
				blocked = i
			}
			continue
		}
		group.Ranks[node.ID] = mgmt
		if own[i] != nil {
			group.FellBack = true
		}
	}
	if blocked < 0 {
		return group, nil
	}
	node := nodes[blocked]
	if own[blocked] == nil {
		return Group{}, &GroupError{Reason: ComputeNone, Node: node}
	}
	for i, other := range nodes {
		switch {
		case i == blocked:
		case own[i] == nil:
			return Group{}, &GroupError{Reason: ComputeMixed, Node: node, Other: other}
		case own[i].Network.ID != own[blocked].Network.ID:
			return Group{}, &GroupError{Reason: ComputeMismatch, Node: node, Other: other, Network: own[blocked].Network.Name, Expected: own[i].Network.Name}
		}
	}
	return Group{}, &GroupError{Reason: ComputeNone, Node: node}
}
