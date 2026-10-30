package api

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// selectedSharedWriter validates the exact requested node. Failure never
// substitutes a different writer, whose network access may be different.
func (a *apiHandlers) selectedSharedWriter(storage *store.Storage, model *store.Model, uuid string) (*store.Node, error) {
	unavailable := func() error {
		return productError(ErrorStorageUnavailableOnNode, "storage "+storage.Name+" is not writable on node "+uuid, map[string]any{"storage": storage.Name, "node": uuid})
	}
	node, err := a.d.Store.NodeByUUID(uuid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, unavailable()
	}
	if err != nil {
		return nil, err
	}
	if a.d.Online != nil && !a.d.Online(uuid) {
		return nil, productError(ErrorNodeOffline, "node "+nodeDisplayName(node)+" is offline", map[string]any{"node": nodeDisplayName(node)})
	}
	if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil {
		return nil, err
	} else if !applies {
		return nil, unavailable()
	}
	state, err := a.d.Store.StorageNodeStateFor(storage.ID, node.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, unavailable()
	}
	if err != nil {
		return nil, err
	}
	if state.State != proto.StorageStateReady || !state.Writable {
		return nil, unavailable()
	}
	if err := admitModelDownloadNode(model, node); err != nil {
		return nil, err
	}
	return node, nil
}
