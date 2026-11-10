package api

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// admitDeploymentModel validates the source before a Deployment row is created.
// Local and storage facts are checked even without a metadata hook. The
// shared inspection keeps missing external observations explicit.
func (a *apiHandlers) admitDeploymentModel(ctx context.Context, model *store.Model, rules store.SourceRules, nodes []*store.Node) error {
	if model == nil || len(nodes) == 0 {
		return nil
	}
	_, err := a.inspectDeploymentModel(ctx, model, rules, nodes, true)
	return err
}
