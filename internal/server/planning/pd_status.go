package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PD deployment states, derived from each role's desired and ready counts and
// never stored (ADR 0028).
const (
	PDStateNotReady = "not_ready" // no instance is ready
	PDStatePartial  = "partial"   // something is ready, but some role has nothing ready
	PDStateRunning  = "running"   // every role has at least one ready instance
)

// PDRoleStatus is one role's desired and ready instance counts.
type PDRoleStatus struct {
	Role    string
	TP      int
	Desired int
	Ready   int
}

// PDStatus is a PD deployment's derived state. Short reports that some role
// runs fewer ready instances than desired, which is a notice beside the
// state, not a state of its own.
type PDStatus struct {
	State string
	Roles []PDRoleStatus
	Short bool
}

// pdInstanceReady is whether an instance can take requests: running and
// awake.
func pdInstanceReady(inst store.Instance) bool {
	return inst.State == proto.InstRunning && inst.SleepLevel == 0
}

// PDStatusOf derives the state from the placement and the instance rows.
func PDStatusOf(pd *PDPlacement, instances []store.Instance) PDStatus {
	mode, _ := pdmode.Lookup(pd.Mode)
	status := PDStatus{State: PDStateRunning}
	anyReady := false
	incomplete := GroupsIncomplete(instances)
	for _, role := range mode.Roles {
		row := PDRoleStatus{Role: role, TP: pd.TP(role), Desired: len(pd.Units(role))}
		for _, inst := range instances {
			// A multi-node unit counts once, by its leader.
			if inst.Role == role && !inst.Headless && !incomplete[inst.UUID] && pdInstanceReady(inst) {
				row.Ready++
			}
		}
		switch {
		case row.Ready > 0:
			anyReady = true
		case row.Desired > 0:
			// A role scaled to zero on purpose (plan D14) is not missing.
			status.State = PDStatePartial
		}
		if row.Ready < row.Desired {
			status.Short = true
		}
		status.Roles = append(status.Roles, row)
	}
	if !anyReady {
		status.State = PDStateNotReady
	}
	return status
}
