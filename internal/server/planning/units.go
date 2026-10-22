// units.go is the one shape every deployment reconciles as (ADR 0029): a list
// of units, each an instance of the deployment with a role (empty outside PD)
// and its ranks. Rank 0 leads and serves; the others are headless followers
// on their own nodes, launched with it as one generation.
//
//	ordinary, replicas N  -> N single-rank pool units: no node of their own,
//	                         the controller picks one from the deployment's
//	                         candidate pool each time a replica starts
//	multi-node, nnodes N  -> one unit of N ranks per listed replica (or one
//	                         over the deployment's node order)
//	PD                    -> every unit of every role, followers included

package planning

import (
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Unit is one instance of a deployment as its placement asks for it.
type Unit struct {
	Role string
	// Index is the unit's position among its role's units.
	Index int
	Ranks []UnitRank
}

// UnitRank is where one rank may run: the node the placement names for it,
// or none for a pool rank, which runs on whichever node of the deployment's
// candidate pool the controller picks when it starts; and the GPUs it is
// pinned to, if any.
type UnitRank struct {
	Nodes []string
	GPUs  []int
}

// MultiNode reports whether the unit spans nodes.
func (u Unit) MultiNode() bool { return len(u.Ranks) > 1 }

// Pool reports whether the unit is a single rank with no node of its own: an
// ordinary single-node replica, placed from the candidate pool at launch.
// Where an instance of it already runs is never held against it - a node
// that stops satisfying the requirements keeps the replica it has (the
// requirements bind new launches only).
func (u Unit) Pool() bool { return len(u.Ranks) == 1 && len(u.Ranks[0].Nodes) == 0 }

// Key identifies interchangeable units, for matching and for the retry
// budget: same role, same ranks.
func (u Unit) Key() string {
	key := u.Role
	for _, rank := range u.Ranks {
		key += "|" + rank.key()
	}
	return key
}

// FollowersKey identifies the unit's follower ranks. A multi-node leader
// records it (UnitRecord), so it is matched to the unit it was launched for.
func (u Unit) FollowersKey() string {
	if len(u.Ranks) < 2 {
		return ""
	}
	parts := make([]string, 0, len(u.Ranks)-1)
	for _, rank := range u.Ranks[1:] {
		parts = append(parts, rank.key())
	}
	return strings.Join(parts, ";")
}

func (r UnitRank) key() string {
	nodes := append([]string(nil), r.Nodes...)
	sort.Strings(nodes)
	return strings.Join(nodes, ",") + fmt.Sprint(sortedGPUs(r.GPUs))
}

// accepts reports whether the rank may run on node with these GPUs: one of
// its nodes (any node for a pool rank), and its pinned GPUs when it pins any.
func (r UnitRank) accepts(node string, gpus []int) bool {
	found := len(r.Nodes) == 0
	for _, candidate := range r.Nodes {
		if candidate == node {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	return len(r.GPUs) == 0 || fmt.Sprint(sortedGPUs(r.GPUs)) == fmt.Sprint(sortedGPUs(gpus))
}

func sortedGPUs(gpus []int) []int {
	out := append([]int(nil), gpus...)
	sort.Ints(out)
	return out
}

// DeploymentUnits is the deployment's units: its PD placement's when it has
// one (in the mode's role order), else one unit per listed replica of a
// multi-node deployment (ReplicaUnitsOf), else one pool unit per replica.
// nodes are the deployment's node UUIDs in their stored order; a single-node
// deployment's are not read, its node list being only where it was created.
func DeploymentUnits(dep *store.Deployment, pd *PDPlacement, nodes []string) []Unit {
	if pd != nil {
		mode, _ := pdmode.Lookup(pd.Mode)
		return pd.UnitsOf(mode)
	}
	if dep.NNodes > 1 {
		var units []Unit
		for index, replica := range ReplicaUnitsOf(dep, nodes) {
			unit := Unit{Index: index}
			for _, rank := range replica.Ranks() {
				unit.Ranks = append(unit.Ranks, UnitRank{Nodes: []string{rank.NodeUUID}})
			}
			units = append(units, unit)
		}
		return units
	}
	replicas := max(dep.Replicas, 1)
	units := make([]Unit, replicas)
	for i := range units {
		units[i] = Unit{Index: i, Ranks: []UnitRank{{}}}
	}
	return units
}

// UnitsOfDeployment is DeploymentUnits over the deployment's stored node
// list, for a caller holding the node UUIDs by ID.
func UnitsOfDeployment(dep *store.Deployment, pd *PDPlacement, nodeUUIDs map[uint]string) []Unit {
	var nodes []string
	for _, id := range dep.NodeIDList() {
		if uuid := nodeUUIDs[id]; uuid != "" {
			nodes = append(nodes, uuid)
		}
	}
	return DeploymentUnits(dep, pd, nodes)
}

// UnitsOf is the placement's units in the mode's role order.
func (p *PDPlacement) UnitsOf(mode pdmode.Mode) []Unit {
	var out []Unit
	for _, role := range mode.Roles {
		for index, unit := range p.Units(role) {
			u := Unit{Role: role, Index: index}
			for _, rank := range unit.Ranks() {
				u.Ranks = append(u.Ranks, UnitRank{Nodes: []string{rank.NodeUUID}, GPUs: rank.GPUIndexes})
			}
			out = append(out, u)
		}
	}
	return out
}
