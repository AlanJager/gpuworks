package planning

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func unitExpl(followers string) string {
	return fmt.Sprintf(`{"pd":{"role":"decode"},"unit":{"followers":%q}}`, followers)
}

func TestMatchUnitsFollowsGenerations(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-nixl")
	unit := PDUnit{NodeUUID: "a", Followers: []PDRank{{NodeUUID: "b"}}}
	pd := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {Units: []PDUnit{{NodeUUID: "a"}}},
		"decode":  {TP: 2, Units: []PDUnit{unit}},
	}}
	units := pd.UnitsOf(mode)
	decodeUnit := units[0]
	nodes := map[uint]string{1: "a", 2: "b", 3: "c"}
	insts := []store.Instance{
		{UUID: "leader", Role: "decode", NodeID: 1, State: proto.InstRunning, NNodes: 2, DistGroup: "g", DistAttempt: 2, ScheduleExpl: unitExpl(decodeUnit.FollowersKey())},
		{UUID: "follower", Role: "decode", NodeID: 2, State: proto.InstRunning, NNodes: 2, DistGroup: "g", DistAttempt: 2, NodeRank: 1, Headless: true},
		// The previous generation of the same group: its follower is not the
		// current leader's.
		{UUID: "stale-follower", Role: "decode", NodeID: 2, State: proto.InstRunning, NNodes: 2, DistGroup: "g", DistAttempt: 1, NodeRank: 1, Headless: true},
		// A leader launched for other followers no longer fits: it drains,
		// and its follower stays until it is gone.
		{UUID: "old-leader", Role: "decode", NodeID: 1, State: proto.InstRunning, NNodes: 2, DistGroup: "g0", DistAttempt: 1, ScheduleExpl: unitExpl("c[]")},
		{UUID: "old-follower", Role: "decode", NodeID: 3, State: proto.InstRunning, NNodes: 2, DistGroup: "g0", DistAttempt: 1, NodeRank: 1, Headless: true},
		// A failed leader is the unit's spent attempt.
		{UUID: "failed-leader", Role: "decode", NodeID: 1, State: proto.InstFailed, NNodes: 2, DistGroup: "g", DistAttempt: 1, ScheduleExpl: unitExpl(decodeUnit.FollowersKey())},
	}
	match := MatchUnits(units, insts, nodes, "")
	decode := match.Slots[0]
	if decode.Unit.Role != "decode" || decode.Inst == nil || decode.Inst.UUID != "leader" || decode.Followers[0] == nil || decode.Followers[0].UUID != "follower" {
		t.Fatalf("decode slot = %+v", decode)
	}
	if len(match.Excess) != 1 || match.Excess[0].UUID != "old-leader" {
		t.Fatalf("excess = %+v", match.Excess)
	}
	if len(match.Orphans) != 1 || match.Orphans[0].UUID != "stale-follower" {
		t.Fatalf("orphans = %+v", match.Orphans)
	}
	if len(match.Failed[decode.Key]) != 1 || match.Slots[1].Inst != nil {
		t.Fatalf("failed = %+v, prefill = %+v", match.Failed, match.Slots[1])
	}
}

func TestDeploymentUnitsStatesEveryShape(t *testing.T) {
	replicas := DeploymentUnits(&store.Deployment{Replicas: 3}, nil, []string{"a", "b"})
	if len(replicas) != 3 || replicas[0].MultiNode() || !replicas[2].Pool() || len(replicas[2].Ranks[0].Nodes) != 0 || replicas[0].Key() != replicas[2].Key() {
		t.Fatalf("replica units = %+v", replicas)
	}
	if units := DeploymentUnits(&store.Deployment{}, nil, []string{"a"}); len(units) != 1 {
		t.Fatalf("an unstated replica count is one unit: %+v", units)
	}
	group := DeploymentUnits(&store.Deployment{NNodes: 2, Replicas: 1}, nil, []string{"a", "b"})
	if len(group) != 1 || len(group[0].Ranks) != 2 || group[0].Pool() || group[0].Ranks[1].Nodes[0] != "b" || group[0].FollowersKey() != "b[]" {
		t.Fatalf("multi-node unit = %+v", group)
	}
}

// Instances launched before units were recorded carry no unit record; a
// multi-node leader of the same rank count still fills its unit, and an
// ordinary instance its replica, so an upgrade rebuilds nothing.
func TestMatchUnitsKeepsInstancesFromBeforeUnitRecords(t *testing.T) {
	group := DeploymentUnits(&store.Deployment{NNodes: 2, ID: 7}, nil, []string{"a", "b"})
	nodes := map[uint]string{1: "a", 2: "b"}
	insts := []store.Instance{
		{UUID: "leader", NodeID: 1, State: proto.InstRunning, NNodes: 2, DistGroup: "deployment-7", DistAttempt: 3},
		{UUID: "follower", NodeID: 2, State: proto.InstRunning, NNodes: 2, DistGroup: "deployment-7", DistAttempt: 3, NodeRank: 1, Headless: true},
	}
	match := MatchUnits(group, insts, nodes, "")
	if match.Slots[0].Inst == nil || match.Slots[0].Followers[0] == nil || len(match.Excess)+len(match.Orphans) != 0 {
		t.Fatalf("legacy group = %+v", match)
	}
	replica := MatchUnits(DeploymentUnits(&store.Deployment{Replicas: 1}, nil, []string{"a"}), []store.Instance{{UUID: "r", NodeID: 1, State: proto.InstRunning}}, nodes, "")
	if replica.Slots[0].Inst == nil {
		t.Fatalf("legacy replica = %+v", replica)
	}
}

func TestGroupsIncompleteCountsOnlyTheLeadersGeneration(t *testing.T) {
	insts := []store.Instance{
		{UUID: "leader", NNodes: 2, DistGroup: "g", DistAttempt: 2, State: proto.InstRunning},
		{UUID: "old", NNodes: 2, DistGroup: "g", DistAttempt: 1, Headless: true, NodeRank: 1, State: proto.InstRunning},
	}
	if !GroupsIncomplete(insts)["leader"] {
		t.Fatal("a follower of the previous generation completed the leader's")
	}
	insts = append(insts, store.Instance{UUID: "new", NNodes: 2, DistGroup: "g", DistAttempt: 2, Headless: true, NodeRank: 1, State: proto.InstRunning})
	if GroupsIncomplete(insts)["leader"] {
		t.Fatal("a complete generation is reported incomplete")
	}
}

// A failed leader no unit accepts any more is a stale attempt: the unit it
// belonged to left the placement, and its budget goes with it.
func TestMatchUnitsReportsAttemptsOfRemovedUnits(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-nixl")
	pd := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {Units: []PDUnit{{NodeUUID: "a"}}},
		"decode":  {Units: []PDUnit{{NodeUUID: "b"}}},
	}}
	nodes := map[uint]string{1: "a", 2: "b", 3: "c"}
	insts := []store.Instance{
		{UUID: "kept", Role: "prefill", NodeID: 1, State: proto.InstFailed},
		{UUID: "removed-unit", Role: "prefill", NodeID: 3, State: proto.InstFailed, GPUIndexes: "[0]"},
	}
	match := MatchUnits(pd.UnitsOf(mode), insts, nodes, "")
	if len(match.Stale) != 1 || match.Stale[0].UUID != "removed-unit" {
		t.Fatalf("stale = %+v", match.Stale)
	}
	if got := match.Failed[match.Slots[1].Key]; len(got) != 1 || got[0].UUID != "kept" {
		t.Fatalf("prefill attempts = %+v", got)
	}
}

// An attempt records the unit it was launched for: a unit re-added with
// another shape (unpinned instead of pinned) does not inherit its failures,
// although it would accept the same node and GPU.
func TestFailedAttemptsCountAgainstTheUnitTheyWereLaunchedFor(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-nixl")
	pinned := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {Units: []PDUnit{{NodeUUID: "a", GPUIndexes: []int{0}}}},
		"decode":  {Units: []PDUnit{{NodeUUID: "b"}}},
	}}
	unpinned := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {Units: []PDUnit{{NodeUUID: "a"}}},
		"decode":  {Units: []PDUnit{{NodeUUID: "b"}}},
	}}
	pinnedKey := pinned.UnitsOf(mode)[1].Key()
	expl := fmt.Sprintf(`{"unit":{"key":%q}}`, pinnedKey)
	insts := []store.Instance{{UUID: "attempt", Role: "prefill", NodeID: 1, GPUIndexes: "[0]", State: proto.InstFailed, ScheduleExpl: expl}}
	nodes := map[uint]string{1: "a", 2: "b"}

	if match := MatchUnits(pinned.UnitsOf(mode), insts, nodes, ""); len(match.Failed[pinnedKey]) != 1 || len(match.Stale) != 0 {
		t.Fatalf("pinned unit = %+v", match)
	}
	match := MatchUnits(unpinned.UnitsOf(mode), insts, nodes, "")
	if len(match.Stale) != 1 || len(match.Failed[match.Slots[1].Key]) != 0 {
		t.Fatalf("re-added unpinned unit = failed %+v stale %+v", match.Failed, match.Stale)
	}
}

// A draining leader no longer fills its unit, so the unit is open for a
// replacement, while the leader's followers stay with it instead of being
// removed as orphans. A draining single-rank replica frees its unit the same
// way.
func TestDrainingLeadersFreeTheirUnitAndKeepTheirFollowers(t *testing.T) {
	multi := Unit{Ranks: []UnitRank{{Nodes: []string{"a"}}, {Nodes: []string{"b"}}}}
	nodes := map[uint]string{1: "a", 2: "b"}
	insts := []store.Instance{
		{UUID: "leader", NodeID: 1, State: proto.InstDraining, NNodes: 2, DistGroup: "g", DistAttempt: 1, ScheduleExpl: fmt.Sprintf(`{"unit":{"key":%q,"followers":%q}}`, multi.Key(), multi.FollowersKey())},
		{UUID: "follower", NodeID: 2, State: proto.InstRunning, NNodes: 2, DistGroup: "g", DistAttempt: 1, NodeRank: 1, Headless: true},
	}
	match := MatchUnits([]Unit{multi}, insts, nodes, "")
	if match.Slots[0].Inst != nil {
		t.Fatalf("a draining leader still fills its unit: %+v", match.Slots[0])
	}
	if len(match.Draining) != 1 || match.Draining[0].UUID != "leader" || len(match.Excess) != 0 {
		t.Fatalf("draining = %+v, excess = %+v", match.Draining, match.Excess)
	}
	if len(match.Orphans) != 0 {
		t.Fatalf("the draining leader's follower was orphaned: %+v", match.Orphans)
	}
	if len(match.Failed) != 0 || len(match.Stale) != 0 {
		t.Fatalf("draining counted as an attempt: failed=%+v stale=%+v", match.Failed, match.Stale)
	}

	replicas := DeploymentUnits(&store.Deployment{Replicas: 2}, nil, []string{"a"})
	single := []store.Instance{
		{UUID: "stays", NodeID: 1, State: proto.InstRunning},
		{UUID: "leaves", NodeID: 1, State: proto.InstDraining},
	}
	match = MatchUnits(replicas, single, nodes, "")
	if match.Slots[0].Inst == nil || match.Slots[0].Inst.UUID != "stays" || match.Slots[1].Inst != nil {
		t.Fatalf("slots = %+v %+v", match.Slots[0].Inst, match.Slots[1].Inst)
	}
	if !InstanceActive(proto.InstRunning) || InstanceActive(proto.InstDraining) {
		t.Fatal("InstanceActive disagrees about draining")
	}
}
