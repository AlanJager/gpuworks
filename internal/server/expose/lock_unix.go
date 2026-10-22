//go:build linux || darwin

package expose

import (
	"fmt"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive, non-blocking lock. A false result with a nil
// error means another process holds it.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case err == syscall.EWOULDBLOCK, err == syscall.EAGAIN:
		return false, nil
	default:
		return false, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
