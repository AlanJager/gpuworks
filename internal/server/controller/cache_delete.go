package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deleteCacheFile runs while the caller holds the node eviction reservation.
// Neither a failed RPC nor a process restart can expose partially removed
// weights: the durable deleting row remains until the exact intent is acked.
func (c *Controller) deleteCacheFile(ctx context.Context, node *store.Node, row *store.ModelFile) error {
	caller, ok := c.hub.(nodeCaller)
	if !ok {
		return errors.New("node RPC is not configured")
	}
	if !c.hub.Online(node.UUID) {
		return errors.New("node is offline")
	}
	if err := c.store.InvalidateCacheDisk(row.StorageID, node.ID); err != nil {
		return err
	}
	row, err := c.store.BeginCacheDeletion(row)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := caller.CallNode(callCtx, node.UUID, proto.RPCCacheEvict, map[string]string{"path": row.Path, "eviction_id": row.TransferID})
	if err != nil {
		return err
	}
	var result struct {
		Removed    bool   `json:"removed"`
		Path       string `json:"path"`
		EvictionID string `json:"eviction_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || !result.Removed || result.Path != filepath.Clean(row.Path) || result.EvictionID != row.TransferID {
		return errors.New("node did not confirm cache removal")
	}
	if err := c.store.InvalidateCacheDisk(row.StorageID, node.ID); err != nil {
		return err
	}
	return c.store.CompleteCacheDeletion(row)
}
