package api

import "gpuworks.ai/gpuworks/internal/server/store"

// Configured roots belong to server.yaml and cannot be changed through the
// management API. Existing storage definitions never grant a new root.
func (a *apiHandlers) validateStoragePaths(row *store.Storage, targets []store.StorageNode) error {
	roots := a.serverSettings().StorageProbeRoots()
	paths := []string{row.Path, row.CachePath}
	for _, target := range targets {
		paths = append(paths, target.Path, target.CachePath)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := store.CheckStoragePath(p); err != nil {
			return productError(ErrorStoragePathInvalid, err.Error(), nil)
		}
		if len(roots) == 0 {
			continue
		}
		allowed := false
		for _, root := range roots {
			if store.CheckStoragePath(root) == nil && store.StoragePathWithin(root, p) {
				allowed = true
				break
			}
		}
		if !allowed {
			return productError(ErrorStoragePathInvalid, "storage path is outside the configured storage_probe_roots", nil)
		}
	}
	return nil
}
