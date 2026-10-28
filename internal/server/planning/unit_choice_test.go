package planning

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// choiceFacts is a cluster for a choice: nodes by ID with their card count,
// each on the network named for it.
func choiceFacts(nodes map[uint]int, networks map[uint]netplan.Network) UnitSuggestFacts {
	var rows []store.Node
	var resolved netplan.Resolution
	for _, id := range sortedIDs(nodes) {
		rows = append(rows, suggestNode(id, "nvidia", "8.0", nodes[id]))
		if network, ok := networks[id]; ok {
			resolved = append(resolved, netplan.Resolved{NodeID: id, Role: netplan.RoleCompute, Network: network, IP: "10.0.0.1", Source: netplan.SourceNode})
		}
	}
	return UnitSuggestFacts{Nodes: rows, Facts: NodeFacts{Networks: resolved}}
}

func sortedIDs(m map[uint]int) []uint {
	var ids []uint
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func nodeUUIDs(nodes []*store.Node) []string {
	var out []string
	for _, node := range nodes {
		out = append(out, node.UUID)
	}
	return out
}

func acceptAll([]*store.Node) int { return -1 }

var (
	ethernet = netplan.Network{ID: 1, Name: "eth", Type: netplan.TypeHostEthernet}
	rdma     = netplan.Network{ID: 2, Name: "ib", Type: netplan.TypeHostRDMA}
)

// A replica goes on an RDMA network before an Ethernet one, on nodes with the
// model first, and the strategy then picks among the rest: binpack the
// fullest node that still fits, spread the emptiest.
func TestChooseReplicaNodesOrder(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 8, 2: 8, 3: 2, 4: 8, 5: 4},
		map[uint]netplan.Network{1: ethernet, 2: ethernet, 3: rdma, 4: rdma, 5: rdma},
	)
	facts.Ranks, facts.PerRank = 2, 2

	nodes, _, ok := ChooseReplicaNodes(facts, store.PlacementBinpack, nil, acceptAll)
	if !ok || !slices.Equal(nodeUUIDs(nodes), []string{"n-3", "n-5"}) {
		t.Fatalf("binpack = %v %v, want the RDMA group's two fullest nodes", nodeUUIDs(nodes), ok)
	}
	nodes, _, ok = ChooseReplicaNodes(facts, store.PlacementSpread, nil, acceptAll)
	if !ok || !slices.Equal(nodeUUIDs(nodes), []string{"n-4", "n-5"}) {
		t.Fatalf("spread = %v %v, want the RDMA group's two emptiest nodes", nodeUUIDs(nodes), ok)
	}
	// The model on n-4 puts it first whatever the strategy.
	facts.Files = []store.ModelFile{{NodeID: 4, State: "present"}}
	nodes, _, _ = ChooseReplicaNodes(facts, store.PlacementBinpack, nil, acceptAll)
	if nodeUUIDs(nodes)[0] != "n-4" {
		t.Fatalf("with the model on n-4 = %v", nodeUUIDs(nodes))
	}
}

// A node the trial refuses is left out and the next try goes without it; a
// group that runs out falls back to the next group; nothing that plans is
// no choice, with the counts of what was left out.
func TestChooseReplicaNodesTriesPastRefusals(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 4, 2: 4, 3: 4, 4: 1},
		map[uint]netplan.Network{1: rdma, 2: rdma, 3: ethernet, 4: ethernet},
	)
	facts.Ranks, facts.PerRank = 2, 2
	refuse := map[string]bool{"n-1": true}
	trial := func(nodes []*store.Node) int {
		for i, node := range nodes {
			if refuse[node.UUID] {
				return i
			}
		}
		return -1
	}
	if _, _, ok := ChooseReplicaNodes(facts, store.PlacementSpread, nil, trial); ok {
		t.Fatal("chose a replica with n-1 refused: the RDMA group has one node left and the Ethernet group one that fits")
	}
	refuse = map[string]bool{}
	nodes, _, ok := ChooseReplicaNodes(facts, store.PlacementSpread, nil, trial)
	if !ok || !slices.Equal(nodeUUIDs(nodes), []string{"n-1", "n-2"}) {
		t.Fatalf("= %v %v", nodeUUIDs(nodes), ok)
	}
	facts.Nodes = facts.Nodes[2:]
	_, counts, ok := ChooseReplicaNodes(facts, store.PlacementSpread, nil, trial)
	if ok || counts.Busy != 1 || counts.Largest != 1 {
		t.Fatalf("ok=%v counts=%+v, want n-4 busy and a group of one", ok, counts)
	}
}

func acceptPD(map[string][]PDUnit) string { return "" }

func pdNodes(units map[string][]PDUnit, role string) []string {
	var out []string
	for _, unit := range units[role] {
		out = append(out, unit.NodeUUID)
	}
	return out
}

// Binpack puts a whole PD deployment on one node when one has the cards for
// it; spread puts prefill and decode on different nodes.
func TestChoosePDUnitsSameNodeOrApart(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 1, 2: 2, 3: 8},
		map[uint]netplan.Network{1: rdma, 2: rdma, 3: rdma},
	)
	roles := []PDRoleNeed{{Role: "prefill", Units: 1, GPUs: 1}, {Role: "decode", Units: 1, GPUs: 1}}

	units, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementBinpack, acceptPD)
	if !ok || !slices.Equal(pdNodes(units, "prefill"), []string{"n-2"}) || !slices.Equal(pdNodes(units, "decode"), []string{"n-2"}) {
		t.Fatalf("binpack = %v %v, want both on n-2, the fullest node that holds both", units, ok)
	}
	units, _, ok = ChoosePDUnits(facts, roles, nil, store.PlacementSpread, acceptPD)
	if !ok || pdNodes(units, "prefill")[0] == pdNodes(units, "decode")[0] {
		t.Fatalf("spread = %v %v, want prefill and decode apart", units, ok)
	}
	if pdNodes(units, "prefill")[0] != "n-3" {
		t.Fatalf("spread prefill = %v, want the emptiest node first", units)
	}
	for _, role := range []string{"prefill", "decode"} {
		for _, unit := range units[role] {
			if len(unit.GPUIndexes) != 0 {
				t.Fatalf("%s unit pins cards %v; the scheduler picks them at launch", role, unit.GPUIndexes)
			}
		}
	}
}

// With no node holding the whole deployment, binpack fills the fewest nodes;
// the cards a unit takes are counted down for the next.
func TestChoosePDUnitsBinpackAcrossNodes(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 2, 2: 2, 3: 2},
		map[uint]netplan.Network{1: rdma, 2: rdma, 3: rdma},
	)
	roles := []PDRoleNeed{{Role: "prefill", Units: 2, GPUs: 1}, {Role: "decode", Units: 1, GPUs: 2}}
	units, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementBinpack, acceptPD)
	if !ok {
		t.Fatal("no choice")
	}
	used := map[string]int{}
	for _, role := range roles {
		for _, node := range pdNodes(units, role.Role) {
			used[node] += role.GPUs
		}
	}
	if len(used) != 2 {
		t.Fatalf("units = %v, want two nodes filled", units)
	}
	for node, cards := range used {
		if cards > 2 {
			t.Fatalf("%s takes %d cards of 2", node, cards)
		}
	}
}

// The roles exchange KV, so they share one group: a cluster split across two
// networks with neither holding the deployment yields no choice; a refused
// node is tried without.
func TestChoosePDUnitsStaysInOneGroup(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 1, 2: 1},
		map[uint]netplan.Network{1: rdma, 2: ethernet},
	)
	roles := []PDRoleNeed{{Role: "prefill", Units: 1, GPUs: 1}, {Role: "decode", Units: 1, GPUs: 1}}
	if units, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementBinpack, acceptPD); ok {
		t.Fatalf("chose %v across two networks", units)
	}
	facts = choiceFacts(map[uint]int{1: 2, 2: 2, 3: 2}, map[uint]netplan.Network{1: rdma, 2: rdma, 3: rdma})
	refusing := func(units map[string][]PDUnit) string {
		for _, role := range []string{"prefill", "decode"} {
			for _, unit := range units[role] {
				if unit.NodeUUID == "n-1" {
					return "n-1"
				}
			}
		}
		return ""
	}
	units, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementBinpack, refusing)
	if !ok || pdNodes(units, "prefill")[0] == "n-1" || pdNodes(units, "decode")[0] == "n-1" {
		t.Fatalf("= %v %v, want n-1 left out after its refusal", units, ok)
	}
}

// Nodes the operator named take the first ranks; the rest come from the
// group they are in. A named node outside every usable group yields nothing.
func TestChooseReplicaNodesWithNamedNodes(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 4, 2: 4, 3: 4, 4: 4},
		map[uint]netplan.Network{1: rdma, 2: rdma, 3: ethernet, 4: ethernet},
	)
	facts.Ranks, facts.PerRank = 2, 2
	nodes, _, ok := ChooseReplicaNodes(facts, store.PlacementBinpack, []string{"n-3"}, acceptAll)
	if !ok || !slices.Equal(nodeUUIDs(nodes), []string{"n-3", "n-4"}) {
		t.Fatalf("= %v %v, want n-3 leading and n-4 from its network", nodeUUIDs(nodes), ok)
	}
	// A named node that leads another replica cannot lead this one.
	facts.Leaders = map[string]bool{"n-3": true}
	if _, _, ok := ChooseReplicaNodes(facts, store.PlacementBinpack, []string{"n-3"}, acceptAll); ok {
		t.Fatal("a replica led from a node that already leads one")
	}
	facts.Leaders = nil
	if _, _, ok := ChooseReplicaNodes(facts, store.PlacementBinpack, []string{"n-9"}, acceptAll); ok {
		t.Fatal("a replica from a node that is in no group")
	}
	// The named node refusing ends the choice: there is nothing else to name.
	refuseNamed := func(nodes []*store.Node) int {
		if nodes[0].UUID == "n-3" {
			return 0
		}
		return -1
	}
	if _, _, ok := ChooseReplicaNodes(facts, store.PlacementBinpack, []string{"n-3"}, refuseNamed); ok {
		t.Fatal("chose past a named node that refused")
	}
}

// Units added to a PD deployment join the group its placed units are in,
// and under binpack the nodes those units already use first.
func TestChoosePDUnitsBesideFixedUnits(t *testing.T) {
	facts := choiceFacts(
		map[uint]int{1: 4, 2: 8, 3: 8},
		map[uint]netplan.Network{1: ethernet, 2: rdma, 3: rdma},
	)
	fixed := map[string][]PDUnit{"prefill": {{NodeUUID: "n-1"}}}
	roles := []PDRoleNeed{{Role: "decode", Units: 1, GPUs: 1}}
	units, _, ok := ChoosePDUnits(facts, roles, fixed, store.PlacementBinpack, acceptPD)
	if !ok || !slices.Equal(pdNodes(units, "decode"), []string{"n-1"}) || !slices.Equal(pdNodes(units, "prefill"), []string{"n-1"}) {
		t.Fatalf("= %v %v, want the decode beside the prefill on n-1 (its network, fewer free cards)", units, ok)
	}
	// Spread prefers another node for the decode, but keeping apart is a
	// preference: the only node on the prefill's network still takes it.
	units, _, ok = ChoosePDUnits(facts, roles, fixed, store.PlacementSpread, acceptPD)
	if !ok || !slices.Equal(pdNodes(units, "decode"), []string{"n-1"}) {
		t.Fatalf("spread = %v %v", units, ok)
	}
}
