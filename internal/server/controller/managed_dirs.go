package controller

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// releaseManagedDirs tells the nodes a deleted deployment's managed
// directories are released, when its template declared any (ADR 0077). The
// row is gone by now; dep is what the delete read before removing it.
func (c *Controller) releaseManagedDirs(dep *store.Deployment) {
	if dep == nil || !deploy.SnapshotDeclaresManagedDirs(dep.CatalogSelectionSnapshot) {
		return
	}
	if hub, ok := c.hub.(interface{ ReleaseDeploymentDirs(uint) int }); ok {
		hub.ReleaseDeploymentDirs(dep.ID)
	}
}
