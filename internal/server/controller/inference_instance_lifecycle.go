package controller

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (c *Controller) inferenceNodeHandlerBeforeCreate(ctx context.Context, node *store.Node, gpus []int, instanceUUID string) error {
	if c.inferenceNodeHandler == nil {
		return nil
	}
	return c.inferenceNodeHandler.BeforeCreate(ctx, provider.InferenceNodeCreateRequest{
		NodeUUID: node.UUID, InstanceUUID: instanceUUID, GPUIndexes: gpus,
	})
}

func (c *Controller) inferenceNodeHandlerCreateFailed(ctx context.Context, node *store.Node, gpus []int, instanceUUID string) error {
	if c.inferenceNodeHandler == nil {
		return nil
	}
	return c.inferenceNodeHandler.OnCreateFailed(ctx, provider.InferenceNodeCreateRequest{
		NodeUUID: node.UUID, InstanceUUID: instanceUUID, GPUIndexes: gpus,
	})
}

func (c *Controller) requiresStopConfirmation(ctx context.Context, inst store.Instance) (bool, error) {
	if c.inferenceNodeHandler == nil {
		return false, nil
	}
	node, err := c.store.NodeByID(inst.NodeID)
	if err != nil {
		return false, err
	}
	return c.inferenceNodeHandler.RequiresStopConfirmation(ctx, node.UUID)
}

func (c *Controller) finalizeFailedAttempts(ctx context.Context, dep *store.Deployment, attempts []store.Instance) (bool, error) {
	for _, inst := range attempts {
		done, err := c.finalizeInstanceRemoval(ctx, inst, false)
		if err != nil || !done {
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			return false, err
		}
	}
	return true, nil
}

// finalizeInstanceRemoval is the single controller exit for an instance being
// retired. Platform handlers get a terminal agent report before the database
// row is removed, so resource leases cannot be orphaned by a retry or reset
// path.
func (c *Controller) finalizeInstanceRemoval(ctx context.Context, inst store.Instance, deleteRow bool) (bool, error) {
	terminal := inst.State == proto.InstStopped || inst.State == proto.InstFailed
	if c.inferenceNodeHandler != nil {
		wait, err := c.requiresStopConfirmation(ctx, inst)
		if err != nil {
			return false, err
		}
		if wait && !terminal {
			return false, nil
		}
		if terminal || !wait {
			node, err := c.store.NodeByID(inst.NodeID)
			if err != nil {
				return false, err
			}
			if err := c.inferenceNodeHandler.AfterInstanceTerminal(ctx, provider.InferenceNodeStopRequest{
				NodeUUID: node.UUID, InstanceUUID: inst.UUID, State: inst.State, Message: inst.Message,
			}); err != nil {
				return false, err
			}
		}
	}
	if deleteRow {
		if err := c.store.DeleteInstance(inst.UUID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	return true, nil
}
