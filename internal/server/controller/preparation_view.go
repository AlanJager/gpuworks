package controller

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// preparationPlacements are the nodes a deployment's pass actually chose
// before any of their instances exist: where its weights are being prepared.
// They are an observation, never a placement instruction. The deployment's
// UpdatedAt versions an entry, so any edit hides it until the pass that the
// edit's own event queues; forgetDeployment drops it.
type preparationPlacements struct {
	mu    sync.Mutex
	nodes map[uint]preparationPlacement
}

type preparationPlacement struct {
	version time.Time
	nodes   []uint
}

func (p *preparationPlacements) set(dep *store.Deployment, nodes []uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(nodes) == 0 {
		delete(p.nodes, dep.ID)
		return
	}
	if p.nodes == nil {
		p.nodes = map[uint]preparationPlacement{}
	}
	p.nodes[dep.ID] = preparationPlacement{dep.UpdatedAt, append([]uint(nil), nodes...)}
}

func (p *preparationPlacements) get(dep *store.Deployment) []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.nodes[dep.ID]
	if !ok || !entry.version.Equal(dep.UpdatedAt) {
		return nil
	}
	return append([]uint(nil), entry.nodes...)
}

func (p *preparationPlacements) forget(id uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.nodes, id)
}

// PreparationNodes are the nodes the deployment's last pass chose and is
// preparing weights on; nil before a pass chose any.
func (c *Controller) PreparationNodes(dep *store.Deployment) []uint {
	return c.placements.get(dep)
}

// CachePhase is the eviction phase the controller is in for a cache entry it
// is making room for, or "" (prepobs.PhaseEvicting / PhaseEvictionFailed).
func (c *Controller) CachePhase(ref store.FileRef) string {
	return c.evictions.Get(ref, c.now())
}
