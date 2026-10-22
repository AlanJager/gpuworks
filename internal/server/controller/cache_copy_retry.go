package controller

import (
	"context"
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// cacheCopyStopWait bounds how long a retry waits for the agent to confirm
// that the failed copy's worker exited. A worker stuck in hard-NFS I/O does
// not confirm, and the retry is refused rather than racing it.
const cacheCopyStopWait = 5 * time.Second

// RetryCacheCopy restarts a failed same-node cache copy an operator asked to
// retry. The new intent is written only after the agent confirmed the old
// worker exited, so a late result of the old copy can neither install nor be
// mistaken for the new one; a concurrent replacement or completion wins the
// transfer-ID compare-and-swap. Refusals are *planning.CacheRetryBlocked.
func (c *Controller) RetryCacheCopy(ctx context.Context, ref store.FileRef) error {
	file, err := c.store.ModelFileAt(ref)
	if err != nil {
		return err
	}
	storage, err := c.store.StorageByID(file.StorageID)
	if err != nil {
		return err
	}
	if !planning.CacheCopyRetryable(storage, file) {
		return &planning.CacheRetryBlocked{Detail: "only a failed cache copy can be retried"}
	}
	node, err := c.store.NodeByID(file.NodeID)
	if err != nil {
		return err
	}
	eff, applies, err := c.store.EffectiveStorageForNode(storage, node)
	if err != nil {
		return err
	}
	caller, callable := c.hub.(nodeCaller)
	if !applies || !callable || !c.hub.Online(node.UUID) {
		return &planning.CacheRetryBlocked{Detail: "the cache node must be connected"}
	}
	// Existing cache entries already reserve their full size. The agent checks
	// actual remaining bytes against statfs again before opening the source.
	plan, err := planning.CachePlan(c.store, storage, eff, node, 0)
	if err != nil {
		return err
	}
	if plan.Refusal != "" || len(plan.Evict) > 0 {
		return &planning.CacheRetryBlocked{Detail: "free cache space before retrying"}
	}
	stopCtx, cancel := context.WithTimeout(ctx, cacheCopyStopWait)
	defer cancel()
	raw, err := caller.CallNode(stopCtx, node.UUID, proto.RPCModelCopyStop, proto.ModelCopyStopParams{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID})
	var stopped proto.ModelCopyStopResult
	if err != nil || json.Unmarshal(raw, &stopped) != nil || !stopped.Stopped {
		return &planning.CacheRetryBlocked{Detail: "the previous copy has not confirmed exit"}
	}
	if err := c.store.RetryModelCopy(file.ID, file.TransferID); err != nil {
		return err
	}
	// The intent is committed; a failed push is repaired by the node's next
	// full sync, so it does not fail the retry.
	if file, err = c.store.ModelFileByID(file.ID); err == nil {
		if model, err := c.store.GetModel(file.ModelID); err == nil {
			if err := c.pushModelFile(node, file, model); err != nil {
				c.log.Warn("push retried cache copy", "node", node.UUID, "model_file", file.ID, "err", err)
			}
		}
	}
	return nil
}
