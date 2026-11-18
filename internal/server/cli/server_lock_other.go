//go:build !unix

package cli

import (
	"os"
	"path/filepath"
)

// Platforms without flock take no server lock: every lock succeeds.
func tryLock(*os.File) error { return nil }

func unlock(*os.File) error { return nil }

func openServerLock(dataDir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dataDir, serverLockFile), os.O_RDONLY|os.O_CREATE, 0o644)
}
