//go:build linux && dqlite

package cluster

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

func databaseUnixSocket(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// Aliases of one storage directory must select the same socket. A storage
	// move changes only this process-local endpoint, not persisted Raft identity.
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("@gpuworks-dqlite-%x", sha256.Sum256([]byte(canonical))), nil
}
