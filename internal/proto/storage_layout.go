package proto

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// The layout inside a shared storage. Everything the platform keeps for
// itself lives under one metadata directory at the storage root, so a model
// directory can never collide with it. The server builds paths in this layout
// (staging) and the agent acts on it (staging, trash, heartbeats), so both
// read the names from here.
const (
	// StorageMetaDir is the platform's directory at the root of a storage.
	StorageMetaDir = ".gpuworks"
	// StorageIncomingDir holds the staging directories of writes in progress,
	// one subdirectory per control plane.
	StorageIncomingDir = "incoming"
	// StorageTrashDir holds model directories that were removed and are
	// waiting for their bytes to be deleted.
	StorageTrashDir = "trash"
	// StorageControlPlanesDir holds one heartbeat file per control plane.
	StorageControlPlanesDir = "control-planes"
)

// StagingRelPath is where a write to a shared storage is assembled before it
// is renamed into place, relative to the storage root: separated per control
// plane so two installations writing the same model never share a staging
// directory. rel is the model's storage-relative target path.
func StagingRelPath(controlPlaneID, rel string) string {
	return path.Join(StorageMetaDir, StorageIncomingDir, controlPlaneID, rel)
}

// JoinWithin joins a relative path onto root and refuses anything that would
// land outside it. rel must name something below root: an empty path, ".", an
// absolute path and any path that climbs out through ".." are errors, so the
// result is never root itself. It does not touch the filesystem (symbolic
// links are not resolved).
func JoinWithin(root, rel string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("root directory is empty")
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("path %q contains a NUL byte", rel)
	}
	clean := filepath.Clean(rel)
	switch {
	case strings.TrimSpace(rel) == "" || clean == ".":
		return "", fmt.Errorf("path %q names no directory below the root", rel)
	case filepath.IsAbs(rel) || filepath.IsAbs(clean):
		return "", fmt.Errorf("path %q must be relative to the root", rel)
	case clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)):
		return "", fmt.Errorf("path %q leaves the root", rel)
	}
	return filepath.Join(root, clean), nil
}
