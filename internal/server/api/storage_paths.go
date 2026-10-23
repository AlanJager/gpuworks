package api

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Configured roots belong to server.yaml and cannot be changed through the
// management API. Existing storage definitions never grant a new root.
// Callers pass only paths this request is setting: a stored path that later
// falls outside storage_probe_roots stays, and is not judged again.
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
			return productError(ErrorStoragePathInvalid, err.Error(), map[string]any{"path": p})
		}
		if store.StoragePathAllowed(roots, p) {
			continue
		}
		allowed := strings.Join(roots, ", ")
		return productError(ErrorStoragePathInvalid,
			fmt.Sprintf("storage path %s is outside the configured storage_probe_roots (%s)", p, allowed),
			map[string]any{"path": p, "allowed_roots": roots})
	}
	return nil
}
