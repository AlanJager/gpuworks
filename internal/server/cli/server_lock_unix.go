//go:build unix

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// tryLock takes f's exclusive flock without waiting.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return errServerLockHeld
	default:
		return fmt.Errorf("lock %s: %w", f.Name(), err)
	}
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// openServerLock opens dataDir's server lock for a maintenance command. It
// opens read-only, which is all flock needs, and creates a missing file with
// the data directory's owner: a root-run command must not leave a file the
// server's own account cannot open for writing its pid.
func openServerLock(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, serverLockFile)
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the server lock %s: %w", path, err)
	}
	if info, err := os.Stat(dataDir); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			if own, err := f.Stat(); err == nil {
				if fst, ok := own.Sys().(*syscall.Stat_t); ok && (fst.Uid != st.Uid || fst.Gid != st.Gid) {
					_ = f.Chown(int(st.Uid), int(st.Gid))
				}
			}
		}
	}
	return f, nil
}
