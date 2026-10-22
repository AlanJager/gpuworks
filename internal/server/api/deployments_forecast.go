package api

import (
	"context"
	"errors"
	"net/url"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Forecast plans, reported by an evaluation for each node a request names.
const (
	forecastPresent  = "present"
	forecastCopy     = "copy"
	forecastPull     = "pull"
	forecastStorage  = "storage"
	forecastDownload = "download"
)

// pinnedNodeUUIDs are the nodes a request names: the ranks of a multi-node
// draft, or the one node of a manual placement. An automatic placement names
// none, and gets no forecast: where it lands is decided at launch.
func pinnedNodeUUIDs(req *dto.DeployRequest) []string {
	if req.NNodes > 1 {
		return req.NodeUUIDs
	}
	if req.NodeUUID != "" {
		return []string{req.NodeUUID}
	}
	return nil
}

// evaluateNodeAcquisition forecasts, for each named node, how it would get
// the image and the weights if it started now: from itself, by copy from a
// node of its sync group, or by pulling/downloading through the pull
// settings. It is the same decision the controller makes at launch
// (controller.deploymentImageDelivery, controller.syncModelWithin), read
// without side effects.
//
// known holds image presence the caller already asked a node about, so the
// same evaluation does not ask the node twice.
func (a *apiHandlers) evaluateNodeAcquisition(ctx context.Context, uuids []string, image string, templateSources []string, model *store.Model, rules store.SourceRules, known map[string]*bool) []dto.AcquisitionNode {
	out := make([]dto.AcquisitionNode, 0, len(uuids))
	copies := a.d.Store.ImageCopies(image, templateSources)
	resolve := a.networks().Resolve
	for _, uuid := range uuids {
		node, err := a.d.Store.NodeByUUID(uuid)
		if err != nil {
			continue
		}
		fact := dto.AcquisitionNode{NodeUUID: node.UUID, NodeName: nodeDisplayName(node)}
		if cached, ok := known[node.UUID]; ok && cached != nil {
			fact.ImageCached = cached
		} else {
			fact.ImageCached, fact.ImageCacheReason = a.imageCacheFact(ctx, image, node)
		}
		a.forecastImage(&fact, node, image, copies, resolve)
		if model != nil {
			a.forecastWeights(&fact, node, model, rules, resolve)
		}
		out = append(out, fact)
	}
	return out
}

func (a *apiHandlers) forecastImage(fact *dto.AcquisitionNode, node *store.Node, image string, copies []string, resolve planning.NodeResolver) {
	if fact.ImageCached != nil && *fact.ImageCached {
		fact.ImagePlan = forecastPresent
		return
	}
	if row, err := a.d.Store.ResolveImageRef(image); err == nil {
		if holders := a.imageHolders(row.ID, node); len(holders) > 0 {
			if peers, err := planning.SyncPeersFor(a.d.Store, resolve, node, holders); err == nil && len(peers) > 0 {
				fact.ImagePlan = forecastCopy
				for i := range holders {
					if holders[i].ID == peers[0].NodeID {
						fact.ImageCopyFrom = nodeDisplayName(&holders[i])
					}
				}
				return
			}
		}
	}
	fact.ImagePlan = forecastPull
	candidates, err := planning.PullCandidatesFor(a.d.Store, resolve, node, image, copies)
	if err != nil {
		fact.ImagePullError = productMessage(err)
		return
	}
	for _, candidate := range candidates {
		fact.ImagePullRefs = append(fact.ImagePullRefs, candidate.Ref)
		fact.ImagePullRanked = fact.ImagePullRanked || candidate.Rank
		// Rule endpoints lead unranked (pullref.PlanCandidates): the node
		// tries them as listed before the copies it orders itself.
		if !candidate.Rank && len(fact.ImagePullRefs) == fact.ImagePullFixed+1 {
			fact.ImagePullFixed++
		}
	}
}

// imageHolders are the online nodes other than target that hold the image.
func (a *apiHandlers) imageHolders(imageID uint, target *store.Node) []store.Node {
	ids, err := a.d.Store.ImageHolders(imageID, target.ID)
	if err != nil {
		return nil
	}
	return a.onlineNodes(ids)
}

// onlineNodes reads the named nodes in one query and keeps the online ones.
func (a *apiHandlers) onlineNodes(ids []uint) []store.Node {
	nodes, err := a.d.Store.NodesForLabelsByID(ids, true)
	if err != nil {
		return nil
	}
	out := nodes[:0]
	for _, node := range nodes {
		if a.d.Online == nil || a.d.Online(node.UUID) {
			out = append(out, node)
		}
	}
	return out
}

func (a *apiHandlers) forecastWeights(fact *dto.AcquisitionNode, node *store.Node, model *store.Model, rules store.SourceRules, resolve planning.NodeResolver) {
	if loc, err := a.d.Store.ReadyLocation(model.ID, node.ID); err == nil && loc != nil {
		fact.WeightsPresent = true
		fact.WeightsPlan = forecastPresent
		return
	}
	src, err := a.d.Store.ModelSourceOn(model.ID, node, rules)
	if err == nil {
		switch src.Action {
		case store.SourceReady:
			fact.WeightsPresent = true
			fact.WeightsPlan = forecastPresent
			return
		case store.SourceFillCache, store.SourceWaitShared:
			fact.WeightsPlan = forecastStorage
			fact.WeightsStorage = src.Location.Storage.Name
			return
		}
	}
	if holders := a.modelHolders(model.ID, node); len(holders) > 0 {
		if peers, err := planning.SyncPeersFor(a.d.Store, resolve, node, holders); err == nil && len(peers) > 0 {
			fact.WeightsPlan = forecastCopy
			for i := range holders {
				if holders[i].ID == peers[0].NodeID {
					fact.WeightsCopyFrom = nodeDisplayName(&holders[i])
				}
			}
			return
		}
	}
	fact.WeightsPlan = forecastDownload
	fact.WeightsProvider = model.Source
	candidates, err := planning.ModelWeightsFetch(a.d.Store, resolve, node, model)
	if err != nil {
		fact.WeightsFetchError = productMessage(err)
		return
	}
	for _, candidate := range candidates {
		if parsed, err := url.Parse(candidate.Base); err == nil && parsed.Host != "" {
			fact.WeightsFetchHosts = append(fact.WeightsFetchHosts, parsed.Host)
		}
	}
}

// modelHolders are the online nodes other than target with a complete copy
// on a local storage, the copies a node-to-node sync reads from.
func (a *apiHandlers) modelHolders(modelID uint, target *store.Node) []store.Node {
	files, err := a.d.Store.ModelFilesForModel(modelID)
	if err != nil {
		return nil
	}
	ids := make([]uint, 0, len(files))
	seen := map[uint]bool{}
	for _, file := range files {
		if file.NodeID == 0 || file.NodeID == target.ID || seen[file.NodeID] || (file.State != proto.MFPresent && file.State != proto.MFRegistered) {
			continue
		}
		seen[file.NodeID] = true
		ids = append(ids, file.NodeID)
	}
	return a.onlineNodes(ids)
}

// productMessage carries an error inside an answer: a ProductError keeps its
// code and params, anything else becomes FIELD_INVALID with the detail.
func productMessage(err error) *dto.ProductMessage {
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		return &dto.ProductMessage{Code: productErr.Code(), Params: productErr.Params(), Message: productErr.Error()}
	}
	return &dto.ProductMessage{Code: ErrorFieldInvalid, Params: map[string]any{"detail": err.Error()}, Message: err.Error()}
}
