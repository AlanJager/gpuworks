package planning

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// lifecycleMu serializes sleep, wake and replica orders, whoever gives them.
// Admission reads
// the ledger and then writes an intent the ledger reads next, so two wakes
// checked at once could both fit a card only one of them gets.
var lifecycleMu sync.Mutex

// LockLifecycle takes the lifecycle order lock and returns its release.
func LockLifecycle() (unlock func()) {
	lifecycleMu.Lock()
	return lifecycleMu.Unlock
}

// DeploymentMatch is how a deployment's instances fill its replicas, read
// from the store: its units over the nodes it lists, matched to insts.
func DeploymentMatch(st *store.Store, dep *store.Deployment, insts []store.Instance) (UnitMatch, error) {
	uuids := map[uint]string{}
	for _, id := range dep.NodeIDList() {
		if node, err := st.NodeByID(id); err == nil {
			uuids[node.ID] = node.UUID
		}
	}
	pd, err := PDOf(dep.Placement)
	if err != nil {
		return UnitMatch{}, err
	}
	return MatchUnits(UnitsOfDeployment(dep, pd, uuids), insts, uuids, dep.PlacementStrategy), nil
}

// WakeGroups splits named instances by the replica they fill, in the order
// they are named; an instance no replica claims is a group of its own. A
// partial wake admits or refuses a group whole, so a multi-node replica comes
// back with all its ranks.
func WakeGroups(match UnitMatch, named []store.Instance) [][]store.Instance {
	var groups [][]store.Instance
	at := map[*UnitSlot]int{}
	for _, in := range named {
		slot := match.SlotOf(in.UUID)
		if slot == nil {
			groups = append(groups, []store.Instance{in})
			continue
		}
		if index, seen := at[slot]; seen {
			groups[index] = append(groups[index], in)
			continue
		}
		at[slot] = len(groups)
		groups = append(groups, []store.Instance{in})
	}
	return groups
}

// Sleepers are a deployment's instances that hold a sleep intent: the ones a
// deployment-wide wake names.
func Sleepers(insts []store.Instance) []store.Instance {
	var out []store.Instance
	for i := range insts {
		if store.HoldsSleepIntent(&insts[i]) {
			out = append(out, insts[i])
		}
	}
	return out
}

// OrderWake wakes the named instances of a deployment once their GPUs can
// take them back (AdmitWake), grouped by replica. With partial, it wakes the
// groups that fit and returns the rest as refusals; without, one refusal
// keeps them all asleep. whole says the order names every sleeper of the
// deployment: when all of them wake, the deployment-wide sleep order is
// withdrawn with them; otherwise only the woken instances' intents change
// (a wake of some instances withdraws the deployment-wide order too, as
// store.OrderInstancesSleep does). The caller holds LockLifecycle.
func OrderWake(st *store.Store, dep *store.Deployment, match UnitMatch, named []store.Instance, partial, whole bool) ([]store.Instance, []WakeRefusal, error) {
	woken, refused, err := AdmitWake(st, WakeGroups(match, named), partial)
	if err != nil {
		return nil, nil, err
	}
	if len(refused) > 0 && (!partial || len(woken) == 0) {
		return nil, refused, nil
	}
	if whole && len(refused) == 0 {
		err = st.SetDeploymentSleep(dep.ID, 0)
	} else if len(woken) > 0 {
		err = st.OrderInstancesSleep(dep.ID, instanceUUIDs(woken), 0, "")
	}
	if err != nil {
		return nil, nil, err
	}
	return woken, refused, nil
}

func instanceUUIDs(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	return out
}
