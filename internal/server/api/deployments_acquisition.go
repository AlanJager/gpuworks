package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// previewSaveToStorageID is the storage an evaluation plans a node against
// saving into: the one the request names, else 0 (the default order of
// store.ModelSourceOn). It is exactly what a create stores. An explicit choice
// is passed through even when it cannot serve the node, so planning says why.
func (a *apiHandlers) previewSaveToStorageID(source *dto.ModelSource, _ *store.Model, _ *store.Node) uint {
	if source == nil {
		return 0
	}
	return source.StorageID
}

// resolveSaveToStorage validates the storage a create names for the model's
// files and returns it. A request that names none returns nil: each node then
// reads whichever copy it has and saves a fetched one by the default order
// (store.ModelSourceOn). A named storage is a preference for the nodes in its
// scope; a node outside it falls back to its local storage, as a launch does
// (ADR 0034). It must still exist, serve the nodes it applies to, and accept
// new files when one of them would have to save the model into it. The answer
// is validated, never substituted (ADR 0018).
func (a *apiHandlers) resolveSaveToStorage(source *dto.ModelSource, model *store.Model, targets []*store.Node) (*store.Storage, error) {
	requested := uint(0)
	if source != nil {
		requested = source.StorageID
	}
	if requested == 0 {
		return nil, nil
	}
	storage, err := a.d.Store.StorageByID(requested)
	if err != nil {
		return nil, productError(ErrorAcquisitionCandidateStale, "the selected storage no longer exists", map[string]any{"storage_id": requested})
	}
	nodes := uniqueNodes(targets)
	if len(nodes) == 0 || model == nil {
		return storage, nil
	}
	if err := a.checkStorageServes(storage, model, nodes, false); err != nil {
		return nil, err
	}
	var inScope []*store.Node
	for _, node := range nodes {
		if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil {
			return nil, err
		} else if applies {
			inScope = append(inScope, node)
		}
	}
	if len(inScope) == 0 || a.storageHoldsModel(storage, model, inScope) {
		return storage, nil
	}
	if err := a.d.Store.StorageAcceptsWrites(storage); err != nil {
		code := ErrorStorageDisabled
		if errors.Is(err, store.ErrStorageNotWritable) {
			code = ErrorStorageNotWritable
		}
		return nil, productError(code, "storage "+storage.Name+" does not accept new model files", map[string]any{"storage": storage.Name})
	}
	return storage, nil
}

// storageHoldsModel reports whether every target node can already read the
// model from storage.
func (a *apiHandlers) storageHoldsModel(storage *store.Storage, model *store.Model, nodes []*store.Node) bool {
	for _, node := range nodes {
		loc, err := a.d.Store.ModelLocation(model.ID, storage.ID, node.ID)
		if err != nil || loc.Reason != "" {
			return false
		}
	}
	return true
}

// checkStorageServes validates the source on each target. A save-to preference
// only applies to nodes in its storage's scope; other nodes follow
// ModelSourceOn's local fallback. present requires the model there: on the node
// for a local storage, in the storage for a shared one (an nfs_cache storage
// copies it into the node cache at launch).
func (a *apiHandlers) checkStorageServes(storage *store.Storage, model *store.Model, nodes []*store.Node, present bool) error {
	for _, node := range nodes {
		loc, err := a.d.Store.ModelLocation(model.ID, storage.ID, node.ID)
		if err != nil {
			return err
		}
		switch loc.Reason {
		case "":
			continue
		case store.LocationStorageNotOnNode:
			if !present {
				continue
			}
			fallthrough
		case store.LocationStorageUnavailable, store.LocationStorageNotFound:
			return productError(ErrorStorageUnavailableOnNode, "storage "+storage.Name+" is not usable on node "+nodeDisplayName(node), map[string]any{"storage": storage.Name, "node": node.UUID, "reason": loc.Reason})
		case store.LocationNotCached:
			continue
		default:
			if present {
				return productError(ErrorAcquisitionCandidateStale, "the model is no longer complete in the selected storage; choose another source", map[string]any{"node": node.UUID, "model_id": model.ID, "storage": storage.Name})
			}
		}
	}
	return nil
}

// startSharedModelDownload starts the one download a shared storage needs
// when a create saves the model there and the storage does not hold it yet:
// every target reads the model from that storage, so it is fetched once,
// through a writer node, rather than by each node. A local storage needs
// nothing here; each node gets the model when it starts (from a node of its
// sync group, else by downloading it).
func (a *apiHandlers) startSharedModelDownload(model *store.Model, storage *store.Storage, targets []*store.Node) (int, error) {
	if model == nil || storage == nil || !proto.StorageShared(storage.Type) || a.storageHoldsModel(storage, model, uniqueNodes(targets)) {
		return http.StatusOK, nil
	}
	if _, status, err := a.startSharedDownload(model, storage); err != nil {
		return status, err
	}
	return http.StatusOK, nil
}

func uniqueNodes(nodes []*store.Node) []*store.Node {
	seen := make(map[uint]struct{}, len(nodes))
	out := make([]*store.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if _, ok := seen[node.ID]; ok {
			continue
		}
		seen[node.ID] = struct{}{}
		out = append(out, node)
	}
	return out
}
