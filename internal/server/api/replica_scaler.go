package api

import (
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// scheduledReplicasRefusal says why a schedule cannot set a deployment's
// replica count, or nil when it can. A single-node deployment, and a
// multi-node or PD deployment the platform places (units_auto), can: new
// replicas get their nodes the way PUT /deployments/{id}/replicas and PUT
// /deployments/{id}/pd pick them. A deployment whose replicas the operator
// placed by hand names the nodes of every replica, which a count cannot say.
//
// A PD deployment scales each role with its own count, so its order names the
// role (one of its mode's); any other deployment's order names none.
func scheduledReplicasRefusal(dep *store.Deployment, role string) error {
	pd, _ := planning.PDOf(dep.Placement)
	if pd == nil {
		if role != "" {
			return scheduleRoleInvalid("only a PD deployment's order names a role", nil)
		}
	} else {
		mode, ok := pdmode.Lookup(pd.Mode)
		switch {
		case role == "":
			return scheduleRoleInvalid("a PD deployment's order names the role it scales", map[string]any{"roles": pdRoleNames(mode)})
		case ok && !mode.HasRole(role):
			return scheduleRoleInvalid(fmt.Sprintf("the %s mode has no %q role", pd.Mode, role), map[string]any{"roles": pdRoleNames(mode)})
		}
	}
	if (dep.NNodes > 1 || pd != nil) && !dep.UnitsAuto {
		return messages.NewProductError(messages.ErrorScheduleReplicasManualPlacement, "the deployment's replicas are placed by hand; switch it to automatic placement to schedule its replica count", nil)
	}
	return nil
}

func scheduleRoleInvalid(reason string, extra map[string]any) error {
	params := map[string]any{"field": "role", "reason": reason}
	for k, v := range extra {
		params[k] = v
	}
	return messages.NewProductError(messages.ErrorScheduleInvalid, "schedule role: "+reason, params)
}

func pdRoleNames(mode pdmode.Mode) []string { return slices.Clone(mode.Roles) }

// scheduledReplicasBounds are the fewest and most replicas an order of the
// deployment may set: 1 to planning.MaxReplicas, and for a PD role 1 to
// pdmode.MaxUnitsPerRole - or 0 where the mode lets the role be empty
// (prefill: without it every request is decoded alone).
func scheduledReplicasBounds(dep *store.Deployment, role string) (lowest, highest int) {
	pd, _ := planning.PDOf(dep.Placement)
	if pd == nil {
		return 1, planning.MaxReplicas
	}
	if mode, ok := pdmode.Lookup(pd.Mode); ok && mode.MayBeEmpty(role) {
		return 0, pdmode.MaxUnitsPerRole
	}
	return 1, pdmode.MaxUnitsPerRole
}

// ReplicaScaler sets a deployment's replica count for a caller inside the
// server (the scheduling automation) the way the API does for an operator: a
// single-node deployment records the desired count (PUT
// /deployments/{id}/replicas), a multi-node deployment placed automatically
// gets the units that autoReplicaUnits picks (new replicas on nodes chosen as
// at creation, a shrink dropping the newest), and a PD deployment's role gets
// its count through the PD update (PUT /deployments/{id}/pd, applyPDUpdate),
// which also holds a role to its mode's minimum. A refusal is a product error
// with its code: a deployment placed by hand, no placement for a new replica,
// a role below its minimum. The count of a non-PD deployment is clamped to
// 1..planning.MaxReplicas; a PD role's is refused outside its bounds.
func ReplicaScaler(d Deps) func(deploymentID uint, role string, replicas int) error {
	a := &apiHandlers{d: d}
	return func(deploymentID uint, role string, replicas int) error {
		dep, err := a.d.Store.GetDeployment(deploymentID)
		if err != nil {
			return err
		}
		if err := scheduledReplicasRefusal(dep, role); err != nil {
			return err
		}
		if pd, _ := planning.PDOf(dep.Placement); pd != nil {
			return a.scalePDRole(dep, pd, role, replicas)
		}
		replicas = min(max(replicas, 1), planning.MaxReplicas)
		if max(dep.Replicas, 1) == replicas {
			return nil
		}
		if dep.NNodes > 1 {
			units, _, err := a.autoReplicaUnits(dep, replicas, nil, nil)
			if err != nil {
				return err
			}
			_, err = a.applyReplicaUnits(dep, units)
			return err
		}
		return a.d.Store.ScaleDeploymentReplicas(dep.ID, dep.Replicas, replicas, nil)
	}
}

// scalePDRole gives one role of a PD deployment placed by the platform a
// count of units: a shrink drops the newest, a grow adds units with no node,
// which the PD update places beside the ones that are.
func (a *apiHandlers) scalePDRole(dep *store.Deployment, pd *planning.PDPlacement, role string, replicas int) error {
	lowest, highest := scheduledReplicasBounds(dep, role)
	if replicas < lowest || replicas > highest {
		return scheduleReplicasOutOfBounds(role, replicas, lowest, highest)
	}
	units := slices.Clone(pd.Units(role))
	if len(units) == replicas {
		return nil
	}
	if len(units) > replicas {
		units = units[:replicas]
	}
	for len(units) < replicas {
		units = append(units, planning.PDUnit{})
	}
	shape := pd.Roles[role]
	shape.Units = units
	_, _, err := a.applyPDUpdate(dep, pd, &pdUpdateRequest{Roles: map[string]planning.PDRole{role: shape}})
	return err
}

// scheduleReplicasOutOfBounds refuses a replica count outside the role's
// bounds, with the code the PD update gives a role it cannot leave empty or
// over-fill.
func scheduleReplicasOutOfBounds(role string, replicas, lowest, highest int) error {
	if replicas < lowest {
		return messages.NewProductError(messages.ErrorPDRoleUnitsEmpty, fmt.Sprintf("the %s role needs at least one instance", role), map[string]any{"role": role})
	}
	return messages.NewProductError(messages.ErrorPDRoleUnitsTooMany, fmt.Sprintf("the %s role has more than %d instances", role, highest), map[string]any{"role": role, "maximum": highest})
}
