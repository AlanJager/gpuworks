package app

import (
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// warnUnmanagedStoragePaths logs storages whose path, cache directory, or
// per-node path is outside the roots this process was given. Changing those
// roots replaces the previous list, so a path that was allowed at install
// can become unmanaged. The roots do not restrict storage paths, so this is
// only a notice: such paths can still be stored, edited and resubmitted.
func warnUnmanagedStoragePaths(st *store.Store, roots []string, log *slog.Logger) {
	if st == nil || log == nil || len(roots) == 0 {
		return
	}
	unmanaged, err := st.UnmanagedStoragePaths(roots)
	if err != nil {
		log.Warn("list storages while checking storage_probe_roots", "err", err)
		return
	}
	for _, row := range unmanaged {
		log.Warn("storage path is outside storage_probe_roots",
			"storage", row.Name, "storage_id", row.StorageID, "path", row.Path, "allowed_roots", roots)
	}
}
