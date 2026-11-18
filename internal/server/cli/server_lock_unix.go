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

// processAlive reports whether pid names a live process; one this user may
// not signal is alive too.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// openServerLock opens dataDir's server lock for a maintenance command. It
// opens read-only, which is all flock needs. With create it creates a
// missing file; without, a missing file is an error (fs.ErrNotExist).
func openServerLock(dataDir string, create bool) (*os.File, error) {
	path := filepath.Join(dataDir, serverLockFile)
	flag := os.O_RDONLY
	if create {
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the server lock %s: %w", path, err)
	}
	ownLikeDataDir(f, dataDir)
	return f, nil
}

// ownLikeDataDir gives the lock file the data directory's owner when root
// runs the command: a root-run serve or maintenance command must not leave
// a file the server's own account cannot open for writing its pid.
func ownLikeDataDir(f *os.File, dataDir string) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if own, err := f.Stat(); err == nil {
		if fst, ok := own.Sys().(*syscall.Stat_t); ok && (fst.Uid != st.Uid || fst.Gid != st.Gid) {
			_ = f.Chown(int(st.Uid), int(st.Gid))
		}
	}
}
