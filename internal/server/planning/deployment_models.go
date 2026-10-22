package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentModelIDs is the common dependency set for admission, launch and
// preparation display. Component bindings are immutable model IDs.
func DeploymentModelIDs(dep *store.Deployment) ([]uint, error) {
	layer, err := DeploymentTemplate(dep)
	if err != nil {
		return nil, err
	}
	ids := []uint{dep.ModelID}
	seen := map[uint]bool{dep.ModelID: true}
	for _, component := range layer.Components {
		if component.StoreModelID == 0 {
			return nil, fmt.Errorf("deployment %d component %s has no immutable model binding", dep.ID, component.Name)
		}
		if !seen[component.StoreModelID] {
			ids = append(ids, component.StoreModelID)
			seen[component.StoreModelID] = true
		}
	}
	return ids, nil
}
