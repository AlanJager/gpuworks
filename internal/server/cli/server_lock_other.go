//go:build !unix

package cli

import (
	"os"
	"path/filepath"
)

// Platforms without flock take no server lock: every lock succeeds.
func tryLock(*os.File) error { return nil }

func unlock(*os.File) error { return nil }

func processAlive(int) bool { return true }

func ownLikeDataDir(*os.File, string) {}

func openServerLock(dataDir string, create bool) (*os.File, error) {
	flag := os.O_RDONLY
	if create {
		flag |= os.O_CREATE
	}
	return os.OpenFile(filepath.Join(dataDir, serverLockFile), flag, 0o644)
}
