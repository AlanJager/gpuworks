package store

import "gpuworks.ai/gpuworks/internal/storagerules"

// CheckStoragePath applies the path safety rules shared with the agent.
func CheckStoragePath(p string) error { return storagerules.ValidatePath(p) }

// StoragePathWithin checks whole path components for local storage policy.
func StoragePathWithin(root, p string) bool { return storagerules.Within(root, p) }
