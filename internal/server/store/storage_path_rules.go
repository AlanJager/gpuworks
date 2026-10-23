package store

import "gpuworks.ai/gpuworks/internal/storagerules"

// CheckStoragePath applies the path safety rules shared with the agent.
func CheckStoragePath(p string) error { return storagerules.ValidatePath(p) }

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
		if CheckStoragePath(root) == nil && StoragePathWithin(root, p) {
			return true
		}
	}
	return false
}
