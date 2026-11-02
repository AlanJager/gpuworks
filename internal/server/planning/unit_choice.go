package planning

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Choosing the nodes of a multi-node replica or of a PD deployment's units
// when the platform places them (store.Deployment.UnitsAuto). The candidates
// are SuggestUnitGroups' groups - one compute network, one GPU architecture,
// the rank's cards free - and the order is the deployment's: an RDMA network
// before an Ethernet one, nodes that already have the model first, then the
// placement strategy (binpack takes the node with the fewest free cards that
// still fit, spread the most). Every choice is tried before it is returned:
// trial plans the proposal the way its launch would and names the node that
// refused it, which the next try leaves out.

// unitChoiceGroups and unitChoiceTries bound the trial plans one choice
// costs: a few groups, a few tries in each.
const (
	unitChoiceGroups = 3
	unitChoiceTries  = 3
)

// networkRank orders compute networks for a choice: RDMA (the host's or the
// cards') before Ethernet.
func networkRank(network netplan.Network) int {
	switch network.Type {
	case netplan.TypeHostRDMA, netplan.TypeAcceleratorRDMA:
		return 0
	default:
		return 1
	}
}

// orderChoiceGroups sorts groups for a choice: RDMA networks first, then the
// most nodes with the model, then SuggestUnitGroups' own order.
func orderChoiceGroups(groups []UnitGroup) {
	sort.SliceStable(groups, func(a, b int) bool {
		x, y := groups[a], groups[b]
		if rx, ry := networkRank(x.Network), networkRank(y.Network); rx != ry {
			return rx < ry
		}
		return modelPresent(x.Nodes) > modelPresent(y.Nodes)
	})
}

// orderByStrategy sorts candidates for one rank of need cards: the model
// first, then the strategy's choice of free cards, then node ID.
func orderByStrategy(nodes []PoolCandidate, strategy store.PlacementStrategy) {
	sort.SliceStable(nodes, func(a, b int) bool {
		x, y := nodes[a], nodes[b]
		if x.Model != y.Model {
			return x.Model > y.Model
		}
		if x.FreeGPUs != y.FreeGPUs {
			if strategy == store.PlacementBinpack {
				return x.FreeGPUs < y.FreeGPUs
			}
			return x.FreeGPUs > y.FreeGPUs
		}
		return x.Node.ID < y.Node.ID
	})
}

// ReplicaTrial plans a proposed multi-node replica and returns the index of
// the rank whose node refused it, -1 when every rank plans.
type ReplicaTrial func(nodes []*store.Node) int

// ChooseReplicaNodes picks the nodes of one multi-node replica: f.Ranks
// nodes with f.PerRank free cards each, one of which may lead. fixed are
// nodes the operator named for its first ranks, in rank order; the rest are
// chosen from the group they are in. ok is false when no group yields a
// replica that plans; counts say why nodes were left out.
func ChooseReplicaNodes(f UnitSuggestFacts, strategy store.PlacementStrategy, fixed []string, trial ReplicaTrial) ([]*store.Node, UnitSuggestCounts, bool) {
	f.Strategy = strategy
	groups, counts := SuggestUnitGroups(f)
	orderChoiceGroups(groups)
	if len(fixed) > 0 {
		groups = groupsHolding(groups, fixed)
	}
	for _, group := range groups[:min(len(groups), unitChoiceGroups)] {
		skip := map[string]bool{}
		for try := 0; try < unitChoiceTries; try++ {
			nodes, ok := replicaFromGroup(group, max(f.Ranks, 1), f.Leaders, fixed, skip)
			if !ok {
				break
			}
			refused := trial(nodes)
			if refused < 0 {
				return nodes, counts, true
			}
			if refused < len(fixed) {
				// A node the operator named refused: nothing else to try in
				// this group.
				break
			}
			skip[nodes[refused].UUID] = true
		}
	}
	return nil, counts, false
}

// groupsHolding are the groups that hold every node in uuids.
func groupsHolding(groups []UnitGroup, uuids []string) []UnitGroup {
	var out []UnitGroup
	for _, group := range groups {
		in := map[string]bool{}
		for _, candidate := range group.Nodes {
			in[candidate.Node.UUID] = true
		}
		all := true
		for _, uuid := range uuids {
			all = all && in[uuid]
		}
		if all {
			out = append(out, group)
		}
	}
	return out
}

// replicaFromGroup is a replica of ranks nodes from group: the fixed nodes
// first, in order, then the group's best others not in skip. Without fixed
// nodes it is UnitFromGroup.
func replicaFromGroup(group UnitGroup, ranks int, leaders map[string]bool, fixed []string, skip map[string]bool) ([]*store.Node, bool) {
	if len(fixed) == 0 {
		return UnitFromGroup(group, ranks, leaders, skip)
	}
	byUUID := map[string]*store.Node{}
	for _, candidate := range group.Nodes {
		byUUID[candidate.Node.UUID] = candidate.Node
	}
	nodes := make([]*store.Node, 0, ranks)
	taken := map[string]bool{}
	for _, uuid := range fixed {
		node := byUUID[uuid]
		if node == nil || taken[uuid] {
			return nil, false
		}
		nodes, taken[uuid] = append(nodes, node), true
	}
	if leaders[nodes[0].UUID] {
		return nil, false
	}
	for _, candidate := range group.Nodes {
		if len(nodes) >= ranks {
			break
		}
		if !taken[candidate.Node.UUID] && !skip[candidate.Node.UUID] {
			nodes, taken[candidate.Node.UUID] = append(nodes, candidate.Node), true
		}
	}
	return nodes, len(nodes) == ranks
}

// PDRoleNeed is one PD role a choice places: how many units and how many
// cards each takes on its node.
type PDRoleNeed struct {
	Role  string
	Units int
	GPUs  int
}

// PDTrial plans proposed PD units and returns the node that refused them,
// "" when every unit plans.
type PDTrial func(units map[string][]PDUnit) string

// ChoosePDUnits picks a node for every unit of a PD deployment. The roles
// exchange KV, so all units are chosen inside one group (one compute network,
// one architecture). Under binpack the whole deployment goes on one node when
// one has the cards for all of it - no KV crosses the network - and otherwise
// the units fill the fewest nodes; under spread each unit takes the node with
// the most free cards, a prefill and a decode on different nodes where they
// can be. Units take no pinned cards: the scheduler picks them at launch.
func ChoosePDUnits(f UnitSuggestFacts, roles []PDRoleNeed, fixed map[string][]PDUnit, strategy store.PlacementStrategy, trial PDTrial) (map[string][]PDUnit, UnitSuggestCounts, bool) {
	smallest, total := 0, 0
	for _, role := range roles {
		if role.Units <= 0 {
			continue
		}
		if smallest == 0 || role.GPUs < smallest {
			smallest = role.GPUs
		}
		total += role.Units * role.GPUs
	}
	f.Ranks, f.PerRank, f.Leaders = 1, max(smallest, 1), nil
	groups, counts := SuggestUnitGroups(f)
	orderChoiceGroups(groups)
	if key, ok := fixedGroupKey(f, fixed); ok {
		// The units already placed set the network and architecture the
		// new ones join: they exchange KV with them.
		var same []UnitGroup
		for _, group := range groups {
			if group.Network.ID == key.network && group.Arch == key.arch {
				same = append(same, group)
			}
		}
		groups = same
	}
	for _, group := range groups[:min(len(groups), unitChoiceGroups)] {
		skip := map[string]bool{}
		for try := 0; try < unitChoiceTries; try++ {
			units, ok := assignPDUnits(group, roles, total, fixed, strategy, skip)
			if !ok {
				break
			}
			refused := trial(units)
			if refused == "" {
				return units, counts, true
			}
			skip[refused] = true
		}
	}
	return nil, counts, false
}

// groupKey is a group's compute network and architecture.
type groupKey struct {
	network uint
	arch    string
}

// fixedGroupKey is the group of the first fixed unit's node, when there is
// one.
func fixedGroupKey(f UnitSuggestFacts, fixed map[string][]PDUnit) (groupKey, bool) {
	for _, units := range fixed {
		for _, unit := range units {
			for i := range f.Nodes {
				node := &f.Nodes[i]
				if node.UUID != unit.NodeUUID {
					continue
				}
				compute, ok := f.Facts.Networks.For(node.ID, netplan.RoleCompute)
				if !ok {
					return groupKey{}, false
				}
				return groupKey{network: compute.Network.ID, arch: nodeArch(node)}, true
			}
		}
	}
	return groupKey{}, false
}

// assignPDUnits places every unit of roles on the group's nodes not in skip,
// counting down each node's free cards as units take them. The fixed units
// stay where they are and are returned with the new ones; a node one of them
// is on counts as in use.
func assignPDUnits(group UnitGroup, roles []PDRoleNeed, total int, fixed map[string][]PDUnit, strategy store.PlacementStrategy, skip map[string]bool) (map[string][]PDUnit, bool) {
	var nodes []PoolCandidate
	for _, candidate := range group.Nodes {
		if !skip[candidate.Node.UUID] {
			nodes = append(nodes, candidate)
		}
	}
	orderByStrategy(nodes, strategy)
	out := map[string][]PDUnit{}
	hosts := map[string]map[string]bool{} // node -> roles it already hosts
	for role, units := range fixed {
		for _, unit := range units {
			out[role] = append(out[role], unit)
			if hosts[unit.NodeUUID] == nil {
				hosts[unit.NodeUUID] = map[string]bool{}
			}
			hosts[unit.NodeUUID][role] = true
		}
	}
	if strategy == store.PlacementBinpack && len(fixed) == 0 {
		for _, candidate := range nodes {
			if candidate.FreeGPUs >= total {
				for _, role := range roles {
					for i := 0; i < role.Units; i++ {
						out[role.Role] = append(out[role.Role], PDUnit{NodeUUID: candidate.Node.UUID})
					}
				}
				return out, true
			}
		}
	}
	free := make(map[string]int, len(nodes))
	for _, candidate := range nodes {
		free[candidate.Node.UUID] = candidate.FreeGPUs
	}
	for _, role := range roles {
		for i := 0; i < role.Units; i++ {
			node := pickPDNode(nodes, free, hosts, role, strategy)
			if node == "" {
				return nil, false
			}
			free[node] -= role.GPUs
			if hosts[node] == nil {
				hosts[node] = map[string]bool{}
			}
			hosts[node][role.Role] = true
			out[role.Role] = append(out[role.Role], PDUnit{NodeUUID: node})
		}
	}
	return out, true
}

// pickPDNode is the node one unit of role takes. Binpack keeps filling the
// node already in use with the fewest cards left; spread takes the node with
// the most free cards, preferring one that hosts no other role.
func pickPDNode(nodes []PoolCandidate, free map[string]int, hosts map[string]map[string]bool, role PDRoleNeed, strategy store.PlacementStrategy) string {
	best := ""
	better := func(uuid string) bool {
		if best == "" {
			return true
		}
		if strategy == store.PlacementBinpack {
			usedA, usedB := len(hosts[uuid]) > 0, len(hosts[best]) > 0
			if usedA != usedB {
				return usedA
			}
			return free[uuid] < free[best]
		}
		otherA, otherB := hostsOtherRole(hosts[uuid], role.Role), hostsOtherRole(hosts[best], role.Role)
		if otherA != otherB {
			return !otherA
		}
		return free[uuid] > free[best]
	}
	for _, candidate := range nodes {
		uuid := candidate.Node.UUID
		if free[uuid] < role.GPUs {
			continue
		}
		if better(uuid) {
			best = uuid
		}
	}
	return best
}

func hostsOtherRole(roles map[string]bool, role string) bool {
	for other := range roles {
		if other != role {
			return true
		}
	}
	return false
}
