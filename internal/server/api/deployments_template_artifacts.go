package api

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) resolveTemplateWeightModel(source weightSource, original *store.InferenceModel, engine, quantization string, weightBytes int64) (*store.Model, error) {
	return a.resolveTemplateWeightModelForNode(source, original, engine, quantization, weightBytes, 0, nil)
}

func (a *apiHandlers) resolveTemplateWeightModelForNode(source weightSource, original *store.InferenceModel, engine, quantization string, weightBytes int64, nodeID uint, pinned []uint, explicitProvider ...bool) (*store.Model, error) {
	if source.RepoID == "" || original == nil {
		return nil, store.ErrNotFound
	}
	var resolveErr error
	source, resolveErr = a.resolveWeightRevision(source, nodeID)
	if resolveErr != nil {
		return nil, resolveErr
	}
	existing, findErr := a.d.Store.FindModelArtifact(source.RepoID, source.Provider, source.Revision, source.Subfolder)
	if findErr != nil && findErr != store.ErrNotFound {
		return nil, findErr
	}
	if findErr == nil && nodeID != 0 {
		ready, err := a.modelHasReadyArtifact(existing, nodeID, pinned)
		if err != nil {
			return nil, err
		}
		if ready {
			return existing, nil
		}
	}
	if findErr == nil && nodeID == 0 {
		return existing, nil
	}
	if len(explicitProvider) > 0 && explicitProvider[0] {
		if findErr == nil {
			return existing, nil
		}
		return a.createTemplateWeightModel(source, original, engine, quantization, weightBytes)
	}

	models, err := a.d.Store.ListModels()
	if err != nil {
		return nil, err
	}
	candidates := make([]store.Model, 0, len(models))
	for _, model := range models {
		if templateWeightMatchesModel(source, &model) {
			candidates = append(candidates, model)
		}
	}
	ready, err := a.readyModelArtifacts(candidates, nodeID, pinned)
	if err != nil {
		return nil, err
	}
	var candidate *store.Model
	bestScore := -1
	for index := range candidates {
		item := &candidates[index]
		// A copy the target node can already use outranks every preference; the
		// requested provider and revision then break ties so repeated resolutions
		// of the same request keep choosing the same row.
		score := 0
		if ready[item.ID] {
			score += 1000
		}
		if strings.EqualFold(strings.TrimSpace(item.Source), strings.TrimSpace(source.Provider)) {
			score += 10
		}
		if strings.TrimSpace(item.Revision) == strings.TrimSpace(source.Revision) {
			score += 5
		}
		if score > bestScore {
			candidate, bestScore = item, score
		}
	}
	if candidate != nil {
		return candidate, nil
	}
	if findErr == nil {
		return existing, nil
	}
	return a.createTemplateWeightModel(source, original, engine, quantization, weightBytes)
}

func (a *apiHandlers) modelHasReadyArtifact(model *store.Model, nodeID uint, pinned []uint) (bool, error) {
	if model == nil || nodeID == 0 {
		return false, nil
	}
	ready, err := a.readyModelArtifacts([]store.Model{*model}, nodeID, pinned)
	return ready[model.ID], err
}

func (a *apiHandlers) readyModelArtifacts(models []store.Model, nodeID uint, pinned []uint) (map[uint]bool, error) {
	ready := make(map[uint]bool, len(models))
	if nodeID == 0 || len(models) == 0 {
		return ready, nil
	}
	node, err := a.d.Store.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	sources, err := a.d.Store.ModelSourcesFor(ids, []*store.Node{node})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		ready[id] = sources.Source(id, nodeID, store.SourceRules{Pinned: pinned}).Action == store.SourceReady
	}
	return ready, nil
}
