package api

import (
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentRuntimeStates applies the deployment read policy to an already
// loaded inventory. Deployments waiting for their first instance need model
// preparation and scheduling facts; those facts are loaded once for the batch.
func DeploymentRuntimeStates(st *store.Store, preparation ModelPreparation, deployments []store.Deployment, instances []store.Instance) (map[uint]string, error) {
	byDeployment := make(map[uint][]store.Instance)
	for _, instance := range instances {
		byDeployment[instance.DeploymentID] = append(byDeployment[instance.DeploymentID], instance)
	}
	states := make(map[uint]string, len(deployments))
	var preparing []store.Deployment
	var running []store.Deployment
	for _, deployment := range deployments {
		current := byDeployment[deployment.ID]
		if len(current) != 0 || deployment.State == store.DeployStopped || deployment.State == store.DeployDeleting {
			states[deployment.ID] = deriveRuntimeState(deployment.State, current, nil, false)
			if states[deployment.ID] == "running" {
				running = append(running, deployment)
			}
			continue
		}
		preparing = append(preparing, deployment)
	}
	if len(running) > 0 {
		nodes, err := st.ListNodes()
		if err != nil {
			return nil, err
		}
		byID := make(map[uint]*store.Node, len(nodes))
		for i := range nodes {
			byID[nodes[i].ID] = &nodes[i]
		}
		// This lookup only serves runtime availability; preparation reads remain batched below.
		rows := &listLookup{nodes: byID}
		for i := range running {
			d := &running[i]
			current := byDeployment[d.ID]
			// Runtime derivation cannot need scheduling facts when instances exist.
			states[d.ID] = deploymentRuntimeState(rows, d, current, nil)
		}
	}
	if len(preparing) == 0 {
		return states, nil
	}
	rows, err := newListLookup(st, preparation, preparing, nil)
	if err != nil {
		return nil, err
	}
	for i := range preparing {
		deployment := &preparing[i]
		states[deployment.ID] = deploymentRuntimeState(rows, deployment, nil, recordedModelProgress(rows, deployment))
	}
	return states, nil
}
