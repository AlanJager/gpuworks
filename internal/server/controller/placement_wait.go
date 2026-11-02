package controller

import (
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica that fits once deployments sharing its cards by yield policy give
// them up waits for them (scheduler.ReasonWaitingForYield): the controller
// notes the deployment in its placement waits, which the residency arbiter
// serves like a request held for a sleeping deployment, and plans the replica
// again every placewait.Recheck until the holders have slept and it fits. A
// pool replica's recheck plans the node it waits on first and stops there
// while that node still fits once its holders sleep (pickPoolNode).
// What it waits for is the plan's typed planning.LaunchYield; the refusal's
// params are for display only.

// PlacementWaits are the deployments whose replica waits for others to give
// up cards.
func (c *Controller) PlacementWaits() *placewait.Registry { return c.waits }

// noteYieldWait notes the outcome of planning dep's replica: a refusal that
// waits for holders to yield (yield, set only with
// scheduler.ReasonWaitingForYield) keeps the deployment waiting for those
// cards and looks again soon; any other refusal ends its wait at once, so
// the residency arbiter stops making room for a replica that no longer
// waits for it.
func (c *Controller) noteYieldWait(dep *store.Deployment, reason scheduler.Reason, yield *planning.LaunchYield) {
	if reason.Code != scheduler.ReasonWaitingForYield || yield == nil {
		c.waits.Done(dep.ID)
		return
	}
	cards := placewait.Cards{NodeID: yield.NodeID, GPUIndexes: yield.GPUIndexes}
	c.waits.Wait(dep.ID, cards, yield.HolderInstances(), c.now())
	c.q.addAfter(depKey(dep.ID), placewait.Recheck)
}
