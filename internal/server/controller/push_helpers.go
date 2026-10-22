package controller

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (c *Controller) pushRemoveInstances(nodeUUID string, uuids []string) error {
	return c.hub.PushToNode(nodeUUID, proto.StateUpdate{RemovedInstances: uuids})
}

// pushModelFile sends one row's task to the node that executes it. The task
// comes from store.ModelFileTask and a download's addresses from
// planning.PlanModelFileTask, the same two the full desired state uses
// (hub.planReplay), so an incremental push and a replay cannot disagree. A
// download the pull source rules forbid on node is not sent: the error says
// so (planning.WeightsFetchRefusal turns it into a deployment's refusal).
func (c *Controller) pushModelFile(node *store.Node, mf *store.ModelFile, model *store.Model) error {
	task, ok := c.store.ModelFileTask(mf, model)
	if !ok {
		return nil
	}
	if err := planning.PlanModelFileTask(c.store, c.networks.Resolve, node, &task, model.DisplayName()); err != nil {
		c.log.Warn("model download not sent", "model", model.ID, "node", node.UUID, "err", err)
		return err
	}
	_ = c.hub.PushToNode(node.UUID, proto.StateUpdate{ModelFiles: []proto.ModelFileTask{task}})
	return nil
}
