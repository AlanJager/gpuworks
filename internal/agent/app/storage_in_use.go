package app

import (
	"path/filepath"
	"sync"
)

// pathUses counts the directories something is working in right now. The
// zero value is ready to use.
type pathUses struct {
	mu    sync.Mutex
	paths map[string]int
}

// hold marks path in use until the returned function is called.
func (u *pathUses) hold(path string) (release func()) {
	path = filepath.Clean(path)
	u.mu.Lock()
	if u.paths == nil {
		u.paths = map[string]int{}
	}
	u.paths[path]++
	u.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.paths[path]--; u.paths[path] <= 0 {
				delete(u.paths, path)
			}
		})
	}
}

func (u *pathUses) within(dir string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for path := range u.paths {
		if pathWithin(dir, path) {
			return true
		}
	}
	return false
}

// inUse reports whether anything on this node reads or writes under a
// storage's directory, which is what keeps the agent from forcing a hung
// mount of it off (storagemount: recoverStuck). It counts what the node's
// users answer (storageUsers) and the peer transfers sending from it.
//
// Not counted, deliberately: the short calls that go through the mount
// manager's guard (storageDo: heartbeat, trash sweep, scan page, peers,
// markers, a removal). On a mount that hangs they are blocked too, each has
// already answered its caller with a time-out, and they are exactly the calls
// a forced unmount gives up; counting them would mean a hung mount is always
// in use. Images are not kept on storages.
func (s *storageRuntime) inUse(_ uint, path string) bool {
	clean := filepath.Clean(path)
	for _, uses := range s.users {
		if uses(clean) {
			return true
		}
	}
	return s.reads.within(clean)
}

// holdRead marks path as being read by a peer transfer until the returned
// function is called.
func (s *storageRuntime) holdRead(path string) (release func()) {
	if s == nil {
		return func() {}
	}
	return s.reads.hold(path)
}

// storageUsers are the node's own answers to "does something work under this
// directory":
//
//   - every instance the agent holds, in any state: its model directory and
//     its mounts stay bound into the container while it sleeps, starts,
//     stops or waits to be reclaimed. When the instances cannot be told (the
//     control plane has not sent them yet, or one is being torn down that
//     the agent has no record of any more) every directory counts as used;
//   - model downloads and storage copies in flight;
//   - peer transfers receiving into it.
func storageUsers(n *NodeRuntime) []func(dir string) bool {
	instances := func(dir string) bool {
		if n.engine == nil {
			return false
		}
		specs, settled := n.engine.StorageUsers()
		if !settled {
			return true
		}
		for _, spec := range specs {
			if spec.ModelPath != "" && pathWithin(dir, filepath.Clean(spec.ModelPath)) {
				return true
			}
			for _, mount := range spec.Mounts {
				if mount.Source != "" && pathWithin(dir, filepath.Clean(mount.Source)) {
					return true
				}
			}
		}
		return false
	}
	downloads := func(dir string) bool {
		n.dlMu.Lock()
		defer n.dlMu.Unlock()
		for _, transfer := range n.downloading {
			if transfer.target != "" && pathWithin(dir, filepath.Clean(transfer.target)) {
				return true
			}
		}
		return false
	}
	receiving := func(dir string) bool {
		n.syncMu.Lock()
		defer n.syncMu.Unlock()
		for _, sync := range n.modelSyncs {
			if sync.target != "" && pathWithin(dir, filepath.Clean(sync.target)) {
				return true
			}
		}
		return false
	}
	return []func(dir string) bool{instances, downloads, receiving}
}
