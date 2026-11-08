package controller

import (
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rollRestart advances a rolling restart one replica at a time. While every
// replica is available (filled by a running, complete generation) it drains
// the oldest replica whose generation predates the request; the drained
// replica's slot opens and the launch below fills it with a new generation.
// Once no replica runs a generation from before the request, the restart is
// over. A replica that cannot come back holds the roll: taking another one
// down would leave two unavailable. One that spent its retry budget holds it
// until the operator retries it, and the timeline says so once per request.
func (c *Controller) rollRestart(dep *store.Deployment, insts []store.Instance, match planning.UnitMatch) error {
	since := *dep.RestartRequestedAt
	incomplete := planning.GroupsIncomplete(insts)
	var oldest *store.Instance
	var stalled *planning.UnitSlot
	unavailable := 0
	for i, slot := range match.Slots {
		inst := slot.Inst
		if inst == nil || inst.State != proto.InstRunning || incomplete[inst.UUID] {
			unavailable++
			if inst == nil && len(planning.ChargedAttempts(match.Failed[slot.Key])) >= c.maxAttempts {
				stalled = &match.Slots[i]
			}
		}
		if inst != nil && inst.CreatedAt.Before(since) && (oldest == nil || inst.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = inst
		}
	}
	if oldest == nil {
		c.log.Info("rolling restart done", "dep", dep.ID)
		return c.store.FinishRollingRestart(dep.ID, since)
	}
	if unavailable > 0 {
		if stalled != nil {
			c.recordRollStalled(dep, since, *stalled, match.Failed[stalled.Key])
		}
		c.q.addAfter(depKey(dep.ID), c.retryBackoff)
		return nil
	}
	c.log.Info("rolling restart replaces a replica", "dep", dep.ID, "instance", oldest.UUID)
	// Only while it still fills its replica: an immediate restart of the same
	// replica that landed meanwhile has already removed it.
	err := c.store.TransitionInstances(dep.ID, []string{oldest.UUID}, store.ReplicaFillingStates(), proto.InstDraining, "rolling restart")
	if err != nil && !errors.Is(err, store.ErrStateChanged) {
		return err
	}
	c.q.add(depKey(dep.ID))
	return nil
}

// recordRollStalled notes, once per restart request, that the roll waits on a
// replica that spent its retry budget.
func (c *Controller) recordRollStalled(dep *store.Deployment, since time.Time, slot planning.UnitSlot, failed []store.Instance) {
	noted, err := c.store.HasDeploymentEventSince(dep.ID, store.EventDeploymentRollingStalled, since)
	if err != nil || noted {
		return
	}
	c.log.Warn("rolling restart waits on a replica that gave up", "dep", dep.ID, "role", slot.Unit.Role, "unit", slot.Unit.Index, "failed", len(failed))
	params := map[string]any{"role": slot.Unit.Role, "replica": slot.Unit.Index + 1, "attempts": len(failed)}
	// Where the failed attempts ran, read while they are still there.
	uuids := make([]string, 0, len(failed))
	for _, inst := range failed {
		uuids = append(uuids, inst.UUID)
	}
	if nodes, err := c.store.NodeUUIDsOfInstances(uuids); err == nil && len(nodes) > 0 {
		params[store.EventParamNodeUUIDs] = nodes
	}
	c.store.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentRollingStalled, params)
}
