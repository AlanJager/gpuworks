package api

import (
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// scheduledReplicasRefusal says why a schedule cannot set a deployment's
// replica count, or nil when it can: a single-node deployment, or a multi-node
// one the platform places (units_auto), whose new replicas get their nodes the
// way PUT /deployments/{id}/replicas picks them. A PD deployment scales each
// role with its own count (PUT /deployments/{id}/pd), which one number cannot
// say, and a multi-node deployment whose replicas the operator placed by hand
// names the nodes of every replica.
func scheduledReplicasRefusal(dep *store.Deployment) error {
	if pd, _ := planning.PDOf(dep.Placement); pd != nil {
		return messages.NewProductError(messages.ErrorScheduleReplicasShape, "a PD deployment scales each role with its own count; a schedule cannot set it", nil)
	}
	if dep.NNodes > 1 && !dep.UnitsAuto {
		return messages.NewProductError(messages.ErrorScheduleReplicasManualPlacement, "the deployment's replicas are placed by hand; switch it to automatic placement to schedule its replica count", nil)
	}
	return nil
}

// ReplicaScaler sets a deployment's replica count for a caller inside the
// server (the scheduling automation) the way PUT /deployments/{id}/replicas
// does for a count: a single-node deployment records the desired count, and a
// multi-node deployment placed automatically gets the units that
// autoReplicaUnits picks (new replicas on nodes chosen as at creation, a
// shrink dropping the newest). A refusal is a product error with its code: a
// deployment placed by hand, a PD deployment, no placement for a new replica.
// The count is clamped to 1..planning.MaxReplicas.
func ReplicaScaler(d Deps) func(deploymentID uint, replicas int) error {
	a := &apiHandlers{d: d}
	return func(deploymentID uint, replicas int) error {
		dep, err := a.d.Store.GetDeployment(deploymentID)
		if err != nil {
			return err
		}
		if err := scheduledReplicasRefusal(dep); err != nil {
			return err
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
