package api

import "gpuworks.ai/gpuworks/internal/server/store"

// Callers pass only paths this request is setting. The shared rules protect
// system directories and reject malformed paths without limiting custom roots.
func (a *apiHandlers) validateStoragePaths(row *store.Storage, targets []store.StorageNode) error {
	paths := []string{row.Path, row.CachePath}
	for _, target := range targets {
		paths = append(paths, target.Path, target.CachePath)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := store.CheckStoragePath(p); err != nil {
			return productError(ErrorStoragePathInvalid, err.Error(), map[string]any{"path": p})
		}
	}
	return nil
}
