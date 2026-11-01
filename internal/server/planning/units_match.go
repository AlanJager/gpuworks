package planning

import (
	"encoding/json"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// UnitSlot is one unit and the instances currently filling it. Which
// instances fill which unit is not stored: it is derived from each leader's
// role, node, GPUs and recorded followers, and from each follower's
// generation (ADR 0029).
type UnitSlot struct {
	Unit Unit
	Key  string
	// Inst is the unit's leader.
	Inst *store.Instance
	// Followers holds the leader's generation's follower ranks by rank-1; nil
	// where a rank has no row. Failed rows are included: a failed rank fails
	// its generation.
	Followers []*store.Instance
}

// Role is the slot's unit role.
func (s UnitSlot) Role() string { return s.Unit.Role }

// UnitMatch is how a deployment's instances fill its units.
type UnitMatch struct {
	Slots []UnitSlot
	// Excess are the active leaders no unit claims: they drain before
	// removal. Their followers stay until the leader is gone.
	Excess []store.Instance
	// Orphans are followers whose generation has no active leader any more;
	// they serve nothing and are removed at once.
	Orphans []store.Instance
	// Failed are each unit key's failed leaders: the unit's spent attempts.
	Failed map[string][]store.Instance
	// Stale are failed leaders no unit accepts: the spent attempts of a unit
	// the placement no longer has. They are retired, so a unit added again
	// starts with its whole budget.
	Stale []store.Instance
	// Draining are the leaders the server is taking out (proto.InstDraining):
	// they no longer fill a unit and wait out their requests before removal.
	// Their followers stay with them.
	Draining []store.Instance
}

// NewestAttempt is the most recently created of a replica's failed attempts,
// the one the controller's give-up record and the response's last failure
// refer to. attempts must not be empty.
func NewestAttempt(attempts []store.Instance) store.Instance {
	newest := attempts[0]
	for _, inst := range attempts[1:] {
		if inst.CreatedAt.After(newest.CreatedAt) {
			newest = inst
		}
	}
	return newest
}

// Unfilled says some replica (or unit) has no instance filling it: one is
// still to be placed.
func (m UnitMatch) Unfilled() bool {
	for i := range m.Slots {
		if m.Slots[i].Inst == nil {
			return true
		}
	}
	return false
}

// SlotOf is the replica an instance currently fills, as matched: the slot
// whose leader it is or whose generation it follows; nil for an instance that
// fills none (failed, draining, on its way out, or no longer claimed).
func (m UnitMatch) SlotOf(uuid string) *UnitSlot {
	for i := range m.Slots {
		slot := &m.Slots[i]
		if slot.Inst == nil {
			continue
		}
		if slot.Inst.UUID == uuid {
			return slot
		}
		for _, follower := range slot.Followers {
			if follower != nil && follower.UUID == uuid {
				return slot
			}
		}
	}
	return nil
}

// InstanceActive reports whether an instance occupies its unit
// (proto.InstanceFillsReplica).
func InstanceActive(state string) bool { return proto.InstanceFillsReplica(state) }

// followerLive reports whether a follower row still stands for a process:
// anything not on its way out. Followers are never drained themselves; a
// draining leader keeps its generation's followers.
func followerLive(state string) bool {
	switch state {
	case proto.InstStopped, proto.InstPendingRemoval, proto.InstRemoving:
		return false
	}
	return true
}

// SameGeneration reports whether two rows belong to one launch of a unit:
// the group identity and the attempt their ranks share.
func SameGeneration(a, b store.Instance) bool {
	return a.DistGroup != "" && a.DistGroup == b.DistGroup && a.DistAttempt == b.DistAttempt
}

// leaderAccepts reports whether a leader fills the unit: its role, a node
// and GPUs rank 0 accepts, and the follower ranks it was launched with.
// Leaders from before launch records carry none; such a row fills a
// multi-node unit of the same rank count.
func leaderAccepts(unit Unit, inst *store.Instance, node string) bool {
	if inst.Role != unit.Role || len(unit.Ranks) == 0 || !unit.Ranks[0].accepts(node, instanceGPUs(inst.GPUIndexes)) {
		return false
	}
	record := ReadScheduleRecord(inst.ScheduleExpl)
	if record.Unit == nil && record.PD == nil {
		return max(inst.NNodes, 1) == len(unit.Ranks)
	}
	return record.UnitRecord().Followers == unit.FollowersKey()
}

// MatchUnits pairs every unit with an active leader that fills it, in unit
// order, and each claimed leader with the followers of its generation. The
// controller (what to create or remove) and the gateway (what may take new
// requests) both derive the answer here. An active leader no unit claims is
// draining: it finishes what it holds and takes nothing new.
//
// Which leaders pool units keep when there are more than units decides which
// replicas a scale-down removes; see byCrowding for how strategy shapes it.
func MatchUnits(units []Unit, insts []store.Instance, nodeUUIDs map[uint]string, strategy store.PlacementStrategy) UnitMatch {
	// Running instances claim units first, then the oldest: the gateway
	// only ever sees the running ones, and both must reach the same answer.
	ordered := append([]store.Instance(nil), insts...)
	sort.SliceStable(ordered, func(a, b int) bool {
		ra, rb := ordered[a].State == proto.InstRunning, ordered[b].State == proto.InstRunning
		if ra != rb {
			return ra
		}
		if !ordered[a].CreatedAt.Equal(ordered[b].CreatedAt) {
			return ordered[a].CreatedAt.Before(ordered[b].CreatedAt)
		}
		return ordered[a].ID < ordered[b].ID
	})
	var leaders, followers []*store.Instance
	for i := range ordered {
		if ordered[i].Headless {
			followers = append(followers, &ordered[i])
		} else {
			leaders = append(leaders, &ordered[i])
		}
	}
	accepts := func(unit Unit, inst *store.Instance) bool {
		return leaderAccepts(unit, inst, nodeUUIDs[inst.NodeID])
	}

	out := UnitMatch{Failed: map[string][]store.Instance{}}
	keys := map[string]bool{}
	for _, unit := range units {
		keys[unit.Key()] = true
	}
	poolKey := ""
	for _, unit := range units {
		if unit.Pool() && unit.Role == "" {
			poolKey = unit.Key()
		}
	}
	poolLeaders := byCrowding(leaders, strategy)
	claimed := map[string]bool{}
	assigned := map[string]bool{}
	for _, unit := range units {
		slot := UnitSlot{Unit: unit, Key: unit.Key()}
		candidates := leaders
		if unit.Pool() {
			candidates = poolLeaders
		}
		for _, inst := range candidates {
			if claimed[inst.UUID] || !InstanceActive(inst.State) || !accepts(unit, inst) {
				continue
			}
			claimed[inst.UUID] = true
			slot.Inst = inst
			break
		}
		if slot.Inst != nil && unit.MultiNode() {
			slot.Followers = make([]*store.Instance, len(unit.Ranks)-1)
			for _, f := range followers {
				if SameGeneration(*f, *slot.Inst) && followerLive(f.State) && f.NodeRank >= 1 && f.NodeRank < len(unit.Ranks) && slot.Followers[f.NodeRank-1] == nil {
					slot.Followers[f.NodeRank-1] = f
					assigned[f.UUID] = true
				}
			}
		}
		out.Slots = append(out.Slots, slot)
	}
	// leaving are the leaders on their way out that still keep their
	// followers: draining ones and unclaimed active ones.
	var leaving []*store.Instance
	for _, inst := range leaders {
		if inst.State == proto.InstFailed {
			// An attempt counts against the unit it was launched for; one
			// launched for a unit the placement no longer has is stale.
			if key := ReadScheduleRecord(inst.ScheduleExpl).UnitRecord().Key; key != "" {
				switch {
				case keys[key]:
					out.Failed[key] = append(out.Failed[key], *inst)
				case poolKey != "" && singleRankKey(key):
					// Launched for a single-node replica when its key still
					// named the deployment's node list: the same replica.
					out.Failed[poolKey] = append(out.Failed[poolKey], *inst)
				default:
					out.Stale = append(out.Stale, *inst)
				}
				continue
			}
			matched := false
			for _, slot := range out.Slots {
				if accepts(slot.Unit, inst) {
					out.Failed[slot.Key] = append(out.Failed[slot.Key], *inst)
					matched = true
					break
				}
			}
			if !matched {
				out.Stale = append(out.Stale, *inst)
			}
			continue
		}
		if inst.State == proto.InstDraining {
			out.Draining = append(out.Draining, *inst)
			leaving = append(leaving, inst)
			continue
		}
		if InstanceActive(inst.State) && !claimed[inst.UUID] {
			out.Excess = append(out.Excess, *inst)
			leaving = append(leaving, inst)
		}
	}
	for _, f := range followers {
		if !followerLive(f.State) || assigned[f.UUID] {
			continue
		}
		draining := false
		for _, leader := range leaving {
			if SameGeneration(*f, *leader) {
				draining = true
				break
			}
		}
		if !draining {
			out.Orphans = append(out.Orphans, *f)
		}
	}
	return out
}

// byCrowding orders leaders for the pool units to claim: running first, as
// for every unit, then by how many of the deployment's active leaders share
// the instance's node, then oldest. Under spread the least crowded nodes'
// leaders are kept, so a scale-down removes replicas from the most crowded
// node and the deployment stays spread; under binpack the most crowded are
// kept, so it empties the emptiest node first.
func byCrowding(leaders []*store.Instance, strategy store.PlacementStrategy) []*store.Instance {
	crowd := map[uint]int{}
	for _, inst := range leaders {
		if InstanceActive(inst.State) {
			crowd[inst.NodeID]++
		}
	}
	out := append([]*store.Instance(nil), leaders...)
	sort.SliceStable(out, func(a, b int) bool {
		ra, rb := out[a].State == proto.InstRunning, out[b].State == proto.InstRunning
		if ra != rb {
			return ra
		}
		ca, cb := crowd[out[a].NodeID], crowd[out[b].NodeID]
		if ca != cb {
			if strategy == store.PlacementBinpack {
				return ca > cb
			}
			return ca < cb
		}
		return false // leaders is already oldest first
	})
	return out
}

// singleRankKey reports whether a recorded unit key is an ordinary
// single-rank replica's: no role, and no follower part, which multi-node keys
// always have.
func singleRankKey(key string) bool {
	return strings.HasPrefix(key, "|") && strings.Count(key, "|") == 1
}

// GroupsIncomplete returns the multi-node leaders among the instances whose
// generation's followers are not all running: such a leader cannot serve
// even when it answers. The status, the gateway and the controller read it.
func GroupsIncomplete(instances []store.Instance) map[string]bool {
	out := map[string]bool{}
	for _, leader := range instances {
		if leader.Headless || leader.NNodes <= 1 {
			continue
		}
		running := 0
		for _, f := range instances {
			if f.Headless && f.State == proto.InstRunning && SameGeneration(f, leader) {
				running++
			}
		}
		if running < leader.NNodes-1 {
			out[leader.UUID] = true
		}
	}
	return out
}

func instanceGPUs(raw string) []int {
	var out []int
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
