package api

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rankImage resolves the runtime image a launch runs on one node: the catalog
// resolution of the requested (or template) image against that node's hardware.
// Evaluate shows it, create preflights and persists it, so every stage judges
// the image the launch will actually use rather than the request's raw field.
func (a *apiHandlers) rankImage(engine string, selection templateSelection, requested string, node *store.Node) deploy.ImageResolution {
	target := deploymentImageTarget(engine, "", node)
	target.Override = requested
	target.TrustedOverride = templateOwnsImage(selection, requested)
	return imagecatalog.Resolve(a.d.Store, target)
}
