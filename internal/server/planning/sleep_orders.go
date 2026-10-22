package planning

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SleepRefusal is why a sleep or wake order for some of a deployment's
// instances may not proceed: a stable product code and its parameters.
type SleepRefusal struct {
	Code   string
	Params map[string]any
}

// InstanceSleepRefusal is why the replica an instance fills cannot take a
// sleep (sleeping) or wake order of its own, nil when it can: the deployment
// cannot sleep or the instance fills no replica. A sleep is refused too for
// the last awake replica of a PD role the mode cannot run without; that one
// sleeps only with the whole deployment. An order naming any rank of a
// multi-node replica is the replica's (ReplicaMembers). match is the
// deployment's current unit match.
func InstanceSleepRefusal(dep *store.Deployment, match UnitMatch, inst *store.Instance, sleeping bool) *SleepRefusal {
	if code := SleepEligibility(dep); code != "" {
		return &SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}}
	}
	slot := match.SlotOf(inst.UUID)
	if slot == nil || !store.HoldsSleepIntent(inst) {
		return &SleepRefusal{Code: messages.ErrorInstanceNotServing, Params: map[string]any{"state": inst.State}}
	}
	if sleeping && slot.Inst.Role != "" && !roleMayBeEmpty(dep, slot.Inst.Role) && !otherAwakeInRole(match, slot.Inst) {
		return &SleepRefusal{Code: messages.ErrorSleepRoleCannotEmpty, Params: map[string]any{"role": inst.Role}}
	}
	return nil
}

// ReplicaMembers are the instances of the replica a slot holds, its leader
// first and then its followers by rank: a sleep or wake order for the replica
// is written on every one, as a deployment-wide order is, while only the
// leader's engine acts on it (the followers' sleep with it).
func ReplicaMembers(slot *UnitSlot) []store.Instance {
	members := []store.Instance{*slot.Inst}
	for _, follower := range slot.Followers {
		if follower != nil {
			members = append(members, *follower)
		}
	}
	return members
}

// RoleSleepRefusal is why a PD deployment's role cannot take a sleep
// (sleeping) or wake order of its own, nil when it can: the deployment is not
// PD, the role is not one of its mode's, or (to sleep) the mode cannot run
// without the role.
func RoleSleepRefusal(dep *store.Deployment, role string, sleeping bool) *SleepRefusal {
	mode, ok := pdModeOf(dep)
	if !ok {
		return &SleepRefusal{Code: messages.ErrorPDNotPD}
	}
	if !mode.HasRole(role) {
		return &SleepRefusal{Code: messages.ErrorPDRoleUnknown, Params: map[string]any{"mode": mode.Name, "role": role}}
	}
	if sleeping && !mode.MayBeEmpty(role) {
		return &SleepRefusal{Code: messages.ErrorSleepRoleCannotEmpty, Params: map[string]any{"role": role}}
	}
	return nil
}

// RoleInstances are a PD deployment's instances of a role that take sleep
// orders (store.HoldsSleepIntent), in UUID order.
func RoleInstances(insts []store.Instance, role string) []store.Instance {
	var out []store.Instance
	for i := range insts {
		if insts[i].Role == role && store.HoldsSleepIntent(&insts[i]) {
			out = append(out, insts[i])
		}
	}
	slices.SortFunc(out, func(a, b store.Instance) int {
		if a.UUID < b.UUID {
			return -1
		}
		if a.UUID > b.UUID {
			return 1
		}
		return 0
	})
	return out
}

// RoleMayBeEmpty reports whether a PD deployment's mode runs with no awake
// instance of the role; true outside PD.
func RoleMayBeEmpty(dep *store.Deployment, role string) bool { return roleMayBeEmpty(dep, role) }

func roleMayBeEmpty(dep *store.Deployment, role string) bool {
	mode, ok := pdModeOf(dep)
	return !ok || mode.MayBeEmpty(role)
}

func pdModeOf(dep *store.Deployment) (pdmode.Mode, bool) {
	pd, err := PDOf(dep.Placement)
	if err != nil || pd == nil {
		return pdmode.Mode{}, false
	}
	return pdmode.Lookup(pd.Mode)
}

// otherAwakeInRole reports whether another replica of the instance's role is
// led by a running instance the gateway would route to: not ordered to sleep
// and not asleep.
func otherAwakeInRole(match UnitMatch, inst *store.Instance) bool {
	for _, slot := range match.Slots {
		leader := slot.Inst
		if leader != nil && leader.UUID != inst.UUID && leader.Role == inst.Role && leader.State == proto.InstRunning && !InstanceAsleep(leader) {
			return true
		}
	}
	return false
}
