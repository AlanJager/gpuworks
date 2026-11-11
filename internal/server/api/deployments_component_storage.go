package api

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// templateComponentModels are the model rows of a template's non-primary
// components, in declaration order. The primary is the deployment's own model
// and goes through the create's model checks already.
func (a *apiHandlers) templateComponentModels(components []deploy.ComponentBinding) ([]*store.Model, error) {
	var out []*store.Model
	for _, component := range components {
		if component.Role == "primary" || component.StoreModelID == 0 {
			continue
		}
		model, err := a.d.Store.GetModel(component.StoreModelID)
		if err != nil {
			return nil, err
		}
		out = append(out, model)
	}
	return out, nil
}

// componentSaveTo is the shared storage a create downloads template
// components into when the request names no storage: the one every target
// node already reads the primary model from, so a component no target can
// read yet is fetched once, next to the primary, instead of by each node into
// its local storage (store.ModelSourceOn never starts a write into a shared
// storage; it waits for one under way). It is a create-time action only: the
// deployment keeps the storage the request named, as an evaluation plans it.
//
// It answers nil, leaving the default order, when the request names a
// storage, when every component is already readable on every target, or when
// no shared storage holding the primary serves all targets and accepts new
// files. Among several, the default storage comes first, then storage id
// order, as in ModelSourceOn.
func (a *apiHandlers) componentSaveTo(source *dto.ModelSource, primary *store.Model, components []*store.Model, pinned []uint, targets []*store.Node) (*store.Storage, error) {
	if (source != nil && source.StorageID != 0) || primary == nil || len(components) == 0 {
		return nil, nil
	}
	nodes := uniqueNodes(targets)
	if len(nodes) == 0 {
		return nil, nil
	}
	missing := false
	for _, component := range components {
		for _, node := range nodes {
			src, err := a.d.Store.ModelSourceOn(component.ID, node, store.SourceRules{Pinned: pinned})
			if err != nil {
				return nil, err
			}
			if src.Action != store.SourceReady {
				missing = true
			}
		}
	}
	if !missing {
		return nil, nil
	}
	storages, err := a.d.Store.ListStorages()
	if err != nil {
		return nil, err
	}
	allowed := map[uint]bool{}
	for _, id := range pinned {
		allowed[id] = true
	}
	sort.SliceStable(storages, func(i, j int) bool {
		if storages[i].IsDefault != storages[j].IsDefault {
			return storages[i].IsDefault
		}
		return storages[i].ID < storages[j].ID
	})
	for i := range storages {
		storage := &storages[i]
		if !proto.StorageShared(storage.Type) || (len(allowed) > 0 && !allowed[storage.ID]) {
			continue
		}
		if a.d.Store.StorageAcceptsWrites(storage) != nil {
			continue
		}
		servesAll := true
		for _, node := range nodes {
			if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil {
				return nil, err
			} else if !applies {
				servesAll = false
				break
			}
		}
		if servesAll && a.storageHoldsModel(storage, primary, nodes) {
			return storage, nil
		}
	}
	return nil, nil
}
