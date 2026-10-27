package api

import (
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// partitionReshaped reports whether an edit changes how the deployment's
// cards are partitioned: whole cards against a dGPU slice, or the partition
// backend. A slice's size is not a reshape. The controller tears a reshaped
// deployment's old generation down before it starts the new one (the dGPU
// cache and claims are keyed per instance), so such an edit cannot roll.
func partitionReshaped(dep, next *store.Deployment) bool {
	return (dep.VRAMLimitBytes > 0) != (next.VRAMLimitBytes > 0) || dep.PartitionBackend != next.PartitionBackend || dep.SMLimitPct != next.SMLimitPct
}

// deploymentEditPlan is how an edit with these changes may reach the
// deployment's instances in its current state; reshape is partitionReshaped.
func (a *apiHandlers) deploymentEditPlan(dep *store.Deployment, changes []dto.DeploymentChange, revision string, reshape bool) (dto.DeploymentEditPlan, error) {
	plan := dto.DeploymentEditPlan{Changes: changes, Revision: revision, ApplyOptions: []string{}, DefaultApply: string(store.EditApplyNone)}
	if plan.Changes == nil {
		plan.Changes = []dto.DeploymentChange{}
	}
	for _, change := range changes {
		if change.Layer == dto.DeploymentChangeRestart {
			plan.Restart = true
		}
	}
	if dep.State == store.DeployStopped {
		// A stopped deployment has nothing to replace: the edit is saved,
		// and starting it launches the new configuration.
		plan.CanStart = true
		if plan.Restart {
			plan.ApplyOptions = []string{string(store.EditApplyNone)}
		}
		return plan, nil
	}
	if !plan.Restart {
		return plan, nil
	}
	if reshape {
		// The old generation goes before the new one starts: replacing all
		// at once is the only replacement there is.
		plan.ApplyNote = dto.DeploymentEditNotePartition
		plan.ApplyOptions = []string{string(store.EditApplyAll), string(store.EditApplyNone)}
		plan.DefaultApply = string(store.EditApplyAll)
		if dep.DesiredSleep > 0 {
			// Replacing a deployment under a sleep order wakes it; that is
			// asked for, not defaulted to.
			plan.CanWake, plan.DefaultApply = true, string(store.EditApplyNone)
		}
		return plan, nil
	}
	if dep.DesiredSleep > 0 {
		// Asleep, the instances keep the configuration they started with
		// until they are replaced, which wakes them.
		plan.CanWake = true
		plan.ApplyOptions = []string{string(store.EditApplyNone), string(store.EditApplyAll)}
		return plan, nil
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return plan, err
	}
	match, err := a.deploymentUnitMatch(dep, insts)
	if err != nil {
		return plan, err
	}
	incomplete := planning.GroupsIncomplete(insts)
	serving := 0
	for _, slot := range match.Slots {
		if slot.Inst != nil && slot.Inst.State == proto.InstRunning && !incomplete[slot.Inst.UUID] {
			serving++
		}
	}
	if len(match.Slots) >= 2 && serving >= 1 {
		// Replacing one replica at a time keeps the others serving.
		plan.ApplyOptions = []string{string(store.EditApplyRolling), string(store.EditApplyAll), string(store.EditApplyNone)}
		plan.DefaultApply = string(store.EditApplyRolling)
		return plan, nil
	}
	// One replica, or none serving (starting, failed): replacing them all
	// at once is all a restart can do.
	plan.ApplyOptions = []string{string(store.EditApplyAll), string(store.EditApplyNone)}
	plan.DefaultApply = string(store.EditApplyAll)
	return plan, nil
}

// editApply is the store edit the request asks for under plan, or the
// refusal of an apply, start or wake the plan does not offer.
func editApply(plan dto.DeploymentEditPlan, body dto.DeploymentEditRequest) (store.DeploymentEdit, error) {
	apply := body.Apply
	if apply == "" {
		apply = plan.DefaultApply
	}
	allowed := plan.ApplyOptions
	if !plan.Restart {
		allowed = []string{string(store.EditApplyNone)}
	}
	if !slices.Contains(allowed, apply) {
		return store.DeploymentEdit{}, productError(ErrorDeploymentEditApplyInvalid, "apply is not one this edit offers", map[string]any{"apply": apply, "options": strings.Join(allowed, ", ")})
	}
	if body.Start && !plan.CanStart {
		return store.DeploymentEdit{}, productError(ErrorDeploymentEditApplyInvalid, "start applies only to a stopped deployment", map[string]any{"apply": "start", "options": strings.Join(allowed, ", ")})
	}
	if body.Wake && (!plan.CanWake || apply != string(store.EditApplyAll)) {
		return store.DeploymentEdit{}, productError(ErrorDeploymentEditApplyInvalid, "wake applies only with apply=all to a deployment under a sleep order", map[string]any{"apply": "wake", "options": strings.Join(allowed, ", ")})
	}
	return store.DeploymentEdit{Apply: store.DeploymentEditApply(apply), Start: body.Start, Wake: body.Wake}, nil
}
