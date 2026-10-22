package expose

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// errLockHeld is the one lock failure that means exclusivity is working: a
// live process holds the lock. Every other failure means the question could not
// be answered, and must never be reported as a held lock.
var errLockHeld = errors.New("the lock is held by a live process")

// Lock takes the short-transaction lock around one config or credential
// change. It is held for milliseconds — never across a wait for the network or
// for the operator — so a second process can always get in.
func (s *Store) Lock(timeout time.Duration) (func(), error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return lockPath(filepath.Join(s.root, ".lock"), timeout)
}

// SupervisorLock takes the hosting lock: exactly one server supervises the
// connector for a data dir. Unlike the transaction lock this one is held for
// the server's whole life, so a second server reports that it cannot host
// rather than waiting.
func (s *Store) SupervisorLock() (func(), error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	release, err := lockPath(filepath.Join(s.root, ".supervisor.lock"), time.Millisecond)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("another gpuworks server is already hosting the public entry point for %s: %w", s.root, err)
		}
		return nil, fmt.Errorf("cannot take the hosting lock for %s: %w", s.root, err)
	}
	return release, nil
}

// lockPath takes an exclusive flock on path. The lock is released by closing
// the descriptor, so the returned function is what keeps it alive.
func lockPath(path string, timeout time.Duration) (func(), error) {
	f, err := openStateLock(path)
	if err != nil {
		return nil, err
	}
	// The mode in the open only applies to a file this call creates, and the
	// creator's umask decides what lands on disk: a root-run CLI under umask
	// 022 leaves 0644, and the connector service — which runs as the data
	// dir's owner — can then not open the file at all. The mode is set again
	// on every open, which also repairs a file that already exists with the
	// group write stripped; a process that may not chmod it, being neither the
	// owner nor root, keeps the mode as it is.
	_ = f.Chmod(0o664)
	deadline := time.Now().Add(timeout)
	for {
		ok, err := tryLockFile(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another gpuworks process is operating on %s: %w", filepath.Dir(path), errLockHeld)
		}
		time.Sleep(50 * time.Millisecond)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// SupervisorHeld reports whether a server currently hosts the entry point.
//
// It answers by trying to take the lock and letting it go again: a lock file
// that merely exists proves nothing, and the plan is explicit that the CLI must
// never read a stale observation as a live address. The answer is only ever
// used together with the revision of that observation.
//
// A lock that cannot be examined counts as held: the callers decide whether to
// remove configuration, skip waiting for the service to exit or start a second
// one, and assuming a live service is the answer none of them can turn into
// damage. Callers that need to tell the two apart use SupervisorCheck.
func (s *Store) SupervisorHeld() bool {
	held, err := s.SupervisorCheck()
	return held || err != nil
}

// SupervisorCheck is SupervisorHeld for callers that must tell a held lock from
// an unanswerable one. A lock this process cannot examine — a permission
// failure on the file, most often — is an error, not a second service: a real
// installation reported "a connector service is already running" for an hour
// because a root-run CLI had created the lock 0644 and the service's account
// could not open it.
func (s *Store) SupervisorCheck() (bool, error) {
	release, err := s.SupervisorLock()
	if err == nil {
		release()
		return false, nil
	}
	if errors.Is(err, errLockHeld) {
		return true, nil
	}
	return false, err
}
