package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storageModel is the model row whose files this launch's primary mount
// reads: the rank's own artifact for rank-scoped weights, the deployment's
// model otherwise. Disk admission and its explanation count this one.
func (in Input) storageModel() *store.Model {
	if in.WeightModel != nil {
		return in.WeightModel
	}
	return in.Model
}

// rankWeightModel is the artifact rank reads under rank-scoped weights
// (ADR 0076) and how many ranks the selection binds, or nil and zero for
// weights the same on every rank. The row is the deployment's model with the
// rank's identity: the ranks are one model cut into equal parts, so
// everything but the artifact (its architecture, its per-rank size) is the
// same, and planning reads no row for it.
func rankWeightModel(selection deploy.CatalogSelection, model *store.Model, rank int) (*store.Model, int) {
	if !selection.RankScoped() || model == nil {
		return nil, 0
	}
	for _, weight := range selection.RankWeights {
		if weight.Rank != rank || weight.StoreModelID == 0 {
			continue
		}
		artifact := *model
		artifact.ID, artifact.ArtifactSubfolder = weight.StoreModelID, weight.Subfolder
		return &artifact, len(selection.RankWeights)
	}
	return nil, len(selection.RankWeights)
}
