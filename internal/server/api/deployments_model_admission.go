package api

import (
	"context"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// admitDeploymentModel performs the source check before a Deployment row is
// created. A node that can read the model, or fill its cache from a storage
// that holds it, needs nothing (store.ModelSourceOn, the rule its replicas
// follow); otherwise the model's source is read once, within the fixed
// admission timeout, since a node will have to fetch it. The read is the
// management node's and the same for every node, so one is enough: repeating
// it per node turned a slow source into one timeout per node.
func (a *apiHandlers) admitDeploymentModel(parent context.Context, model *store.Model, rules store.SourceRules, nodes []*store.Node) error {
	if model == nil || len(nodes) == 0 || a.d.FetchModelMeta == nil {
		return nil
	}
	seen := map[uint]bool{}
	read := false
	for _, node := range nodes {
		if node == nil || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		src, err := a.d.Store.ModelSourceOn(model.ID, node, rules)
		if err != nil {
			return err
		}
		switch src.Action {
		case store.SourceReady, store.SourceFillCache, store.SourceWaitShared:
			continue
		}
		// A copy already accepted by the control plane supplies this local
		// destination. Admission may wait for it; launch still requires Ready.
		if file := src.Location.File; src.Action == store.SourceAcquire && store.TransferUnderWay(file) && file.Acquisition == proto.ModelAcquireCopy && file.TransferID != "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.Source), "local") || strings.TrimSpace(model.SourceID) == "" {
			return productError(ErrorModelSourceUnavailable, "model weights are not complete on the selected node", map[string]any{"node_id": node.ID, "model_id": model.ID})
		}
		if read {
			continue
		}
		read = true
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		_, err = a.fetchModelMeta(ctx, model.Source, model.SourceID, model.Revision, model.ArtifactSubfolder, model.SourceURL)
		cancel()
		if err != nil {
			return wrapProductError(ErrorModelSourceUnavailable, "model source is unavailable; deployment was not created", map[string]any{"provider": model.Source, "repo_id": model.SourceID, "revision": model.Revision, "node_id": node.ID}, err)
		}
	}
	return nil
}
