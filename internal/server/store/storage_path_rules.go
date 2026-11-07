package store

import "gpuworks.ai/gpuworks/internal/storagerules"

// CheckStoragePath applies the path form rules shared with the agent and
// refuses p at or below any of the denied directories.
func CheckStoragePath(p string, denied []string) error { return storagerules.ValidatePath(p, denied) }

// ErrStorageDeniedPath is what CheckStoragePath returns for a path at or
// below a denied directory.
var ErrStorageDeniedPath = storagerules.ErrDeniedPath

// CheckStoragePathForm applies only the form rules: absolute, no dot segments
// or control characters, not the filesystem root.
func CheckStoragePathForm(p string) error { return storagerules.CheckPathForm(p) }

// DefaultStorageDeniedPaths is the built-in directory denylist.
func DefaultStorageDeniedPaths() []string { return storagerules.DefaultDeniedPaths() }

// StoragePathWithin checks whole path components for local storage policy.
func StoragePathWithin(root, p string) bool { return storagerules.Within(root, p) }

// StoragePathAllowed reports whether p sits under one of the configured
// storage roots. An empty path or an empty root list is allowed: no roots
// means the server is not enforcing this policy, and a blank path is not a
// new location. Callers still reject an unsafe path through CheckStoragePath.
func StoragePathAllowed(roots []string, p string) bool {
	if p == "" || len(roots) == 0 {
		return true
	}
	for _, root := range roots {
		if storagerules.CheckPathForm(root) == nil && StoragePathWithin(root, p) {
			return true
		}
	}
	return false
}
