package controller

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A versioned reverse index keeps opaque launch documents in Go and avoids
// decoding the fleet for each canceled model/component. Deployment writes
// invalidate it through the store's existing event generation.
type downloadDependencyCache struct {
	mu         sync.Mutex
	generation uint64
	byModel    map[uint][]store.Deployment
}

func (c *Controller) downloadDependents(modelIDs []uint) ([]store.Deployment, error) {
	cache := &c.downloadUsers
	cache.mu.Lock()
	defer cache.mu.Unlock()
	generation := c.store.Bus.Generation("deployment")
	if cache.byModel == nil || cache.generation != generation {
		deps, err := c.store.ListDeployments()
		if err != nil {
			return nil, err
		}
		byModel := map[uint][]store.Deployment{}
		for _, dep := range deps {
			if dep.State != store.DeployActive {
				continue
			}
			ids, err := planning.DeploymentModelIDs(&dep)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				byModel[id] = append(byModel[id], dep)
			}
		}
		cache.byModel, cache.generation = byModel, generation
	}
	seen := map[uint]bool{}
	var out []store.Deployment
	for _, modelID := range modelIDs {
		for _, dep := range cache.byModel[modelID] {
			if !seen[dep.ID] {
				seen[dep.ID] = true
				out = append(out, dep)
			}
		}
	}
	return out, nil
}
