//go:build linux || darwin

package selfinstall

import (
	"fmt"
	"os"
	"syscall"
)

// freeBytes reports the space available to unprivileged writers under path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bsize) * st.Bavail, nil
}

// lockFile takes an exclusive, non-blocking lock. A false result with a nil
// error means another process holds it.
func lockFile(f *os.File) (bool, error) {
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

// processAlive reports whether pid names a running process. A process owned by
// another user is alive but not signalable, which the kernel reports as EPERM.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	return !exitedProcess(pid)
}

// signalProcess sends SIGTERM or SIGKILL to pid.
func signalProcess(pid int, sig string) error {
	switch sig {
	case "KILL":
		return syscall.Kill(pid, syscall.SIGKILL)
	default:
		return syscall.Kill(pid, syscall.SIGTERM)
	}
}
