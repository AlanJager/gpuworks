package store

// UnmanagedStoragePath is a stored path the current storage_probe_roots no
// longer cover. It is derived at startup; nothing persists it.
type UnmanagedStoragePath struct {
	StorageID uint
	Name      string
	Path      string
}

// UnmanagedStoragePaths lists storage, cache, and per-node paths that sit
// outside roots. An empty root list enforces nothing, matching path checks
// on write.
func (s *Store) UnmanagedStoragePaths(roots []string) ([]UnmanagedStoragePath, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	rows, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	var out []UnmanagedStoragePath
	for _, row := range rows {
		paths := []string{row.Path, row.CachePath}
		targets, err := s.StorageNodes(row.ID)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			paths = append(paths, target.Path, target.CachePath)
		}
		seen := map[string]bool{}
		for _, p := range paths {
			if p == "" || seen[p] || StoragePathAllowed(roots, p) {
				continue
			}
			seen[p] = true
			out = append(out, UnmanagedStoragePath{StorageID: row.ID, Name: row.Name, Path: p})
		}
	}
	return out, nil
}
