//go:build !unix

package layout

import "os"

// Lock creates the plugins directory; platforms without flock take no lock.
func (d Dir) Lock() (func() error, error) {
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		return nil, err
	}
	return func() error { return nil }, nil
}

// TryLock is Lock: platforms without flock take no lock, so it is always
// free.
func (d Dir) TryLock() (func() error, bool, error) {
	unlock, err := d.Lock()
	return unlock, err == nil, err
}
