//go:build linux || darwin

package filelock

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// Acquire takes an exclusive nonblocking lock. The caller creates the parent
// directory. The lock file stays in place after release so all contenders keep
// using the same inode. Closing the descriptor, including on process exit,
// releases ownership; the returned release function is safe to call repeatedly.
func Acquire(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			err = ErrBusy
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure lock %s: %w", path, err)
	}
	var once sync.Once
	var closeErr error
	return func() error { once.Do(func() { closeErr = f.Close() }); return closeErr }, nil
}
