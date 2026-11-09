package api

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Callers pass only paths this request is setting. The shared rules reject
// malformed paths and the configured denied directories without limiting
// custom roots.
func (a *apiHandlers) validateStoragePaths(row *store.Storage, targets []store.StorageNode) error {
	denied := a.serverSettings().StorageDeniedPaths()
	paths := []string{row.Path, row.CachePath}
	for _, target := range targets {
		paths = append(paths, target.Path, target.CachePath)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		switch err := store.CheckStoragePath(p, denied); {
		case errors.Is(err, store.ErrStorageDeniedPath):
			return productError(ErrorStoragePathDenied, err.Error(), map[string]any{"path": p})
		case err != nil:
			return productError(ErrorStoragePathInvalid, err.Error(), map[string]any{"path": p})
		}
	}
	return nil
}
