//go:build unix

package layout

import (
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the plugins directory's exclusive lock, so two installs or
// removals (the command line beside a running server) never interleave. It
// blocks until the lock is free; the returned function releases it.
func (d Dir) Lock() (func() error, error) {
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d.Root, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}
