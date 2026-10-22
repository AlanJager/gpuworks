package controller

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// cacheReferencesChanged retries cleanup only where the changed readers lived.
// Delete notifications contain only an ID and arrive after the row is gone.
// Their durable waiting operations are retried by the next node status report
// (or reconnect/startup), rather than waking every cache in the fleet.
func (c *Controller) cacheReferencesChanged(ev bus.Event) {
	var nodeIDs []uint
	switch ev.Topic {
	case "instance":
		inst, err := c.store.InstanceByUUID(ev.Key)
		if err != nil {
			return
		}
		nodeIDs = []uint{inst.NodeID}
	case "deployment":
		id, err := strconv.ParseUint(ev.Key, 10, 64)
		if err != nil {
			return
		}
		dep, err := c.store.GetDeployment(uint(id))
		if err != nil {
			return
		}
		// Include both configured placement and actual instances: a pool or a
		// placement edit can leave readers on nodes absent from the new plan.
		nodeIDs, err = dep.StoredNodeIDs()
		if err != nil {
			return
		}
		nodeIDs = append(nodeIDs, dep.NodeID)
		insts, err := c.store.InstancesForDeployment(dep.ID)
		if err != nil {
			return
		}
		for _, inst := range insts {
			nodeIDs = append(nodeIDs, inst.NodeID)
		}
	default:
		return
	}
	ops, err := c.store.WaitingCacheOperations(nodeIDs)
	if err != nil {
		c.log.Warn("find cache operations for changed references", "topic", ev.Topic, "key", ev.Key, "err", err)
		return
	}
	for _, op := range ops {
		c.cacheQ.add(cacheKey(op.StorageID, op.NodeID))
	}
}
