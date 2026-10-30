package controller

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica that fits once deployments sharing its cards by yield policy give
// them up waits for them (scheduler.ReasonWaitingForYield): the controller
// notes the deployment in its placement waits, which the residency arbiter
// serves like a request held for a sleeping deployment, and plans the replica
// again every yieldRecheck until the holders have slept and it fits.

// yieldRecheck is how often a replica waiting for holders to yield is planned
// again: well within placewait.Fresh, so the note stays current.
const yieldRecheck = 5 * time.Second

// PlacementWaits are the deployments whose replica waits for others to give
// up cards.
func (c *Controller) PlacementWaits() *placewait.Registry { return c.waits }

// noteYieldWait notes a refusal that waits for holders to yield, with the
// instances it waits for, and looks again soon.
func (c *Controller) noteYieldWait(dep *store.Deployment, reason scheduler.Reason) {
	if reason.Code != scheduler.ReasonWaitingForYield {
		return
	}
	instances, _ := reason.Params["instances"].([]string)
	c.waits.Wait(dep.ID, instances, c.now())
	c.q.addAfter(depKey(dep.ID), yieldRecheck)
}
