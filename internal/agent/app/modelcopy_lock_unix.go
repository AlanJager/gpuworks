//go:build linux || darwin

package app

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The advisory lock survives cancellation and is released only when all copy
// I/O has returned. Separate agent processes cannot resume the same target
// while an older process still holds a file descriptor (including stuck I/O).
func lockCopyTarget(target string) (func(), error) {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf(".gpuworks-copy-%x.lock", sha256.Sum256([]byte(filepath.Clean(target))))
	file, err := os.OpenFile(filepath.Join(parent, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("model copy target is held by another writer: %w", err)
	}
	// Never unlink: a waiter may already have opened this inode.
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
