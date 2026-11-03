package selfinstall

import (
	"fmt"
	"os"
	"path/filepath"
)

// sharedApplicationRoot is the historical server install root and also the
// colocated agent's root-owned state directory: its helper binaries, the
// partition libraries injected into GPU containers, probe libraries and the
// model cache. Handing that tree to the service account would let it rewrite
// what the root agent runs, so a server installed directly into it shares only
// its own entries, and the directory itself stays root's.
var sharedApplicationRoot = "/opt/gpuworks"

// installedInSharedRoot reports whether the install directory is the shared
// application root, by path or by what it resolves to.
func (i *Installer) installedInSharedRoot() bool {
	return sameDirectory(i.layout.Dir, sharedApplicationRoot)
}

func sameDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := resolveUninstallPath(a)
	rb, errB := resolveUninstallPath(b)
	return errA == nil && errB == nil && ra == rb
}

// serverEntries are what the server owns inside its install directory. Only
// the existing ones are returned: the first check runs before Install writes
// anything, and the pid and lock files appear only on some paths.
func (l Layout) serverEntries() []string {
	var entries []string
	for _, path := range []string{l.Binary(), l.ConfigPath(), filepath.Join(l.Dir, installJSONFile),
		l.PIDFile(), l.LockPath(), l.TmpDir(), l.BackupsDir} {
		if _, err := os.Lstat(path); err == nil {
			entries = append(entries, path)
		}
	}
	return entries
}

// Validate every root before changing anything. Resolve symlinks before
// deduplicating: data inside the install tree may point to an external disk.
// The shared application root is never a root itself; see sharedApplicationRoot.
func (i *Installer) permissionRoots() ([]string, error) {
	dirs := []string{i.layout.Dir, i.layout.DataDir, i.layout.LogsDir}
	sharedEntries := map[string]bool{}
	if i.installedInSharedRoot() {
		entries := i.layout.serverEntries()
		for _, entry := range entries {
			sharedEntries[entry] = true
		}
		dirs = append(entries, i.layout.DataDir, i.layout.LogsDir)
	}
	var roots []string
	for _, dir := range dirs {
		if err := checkDedicatedDirectory(dir, sharedApplicationRoot); err != nil {
			return nil, fmt.Errorf("refusing recursive permission changes for %s: %w", dir, err)
		}
		root, err := resolveUninstallPath(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve permission directory %s: %w", dir, err)
		}
		if err := checkDedicatedDirectory(root, sharedApplicationRoot); err != nil {
			return nil, fmt.Errorf("refusing recursive permission changes for %s: %w", dir, err)
		}
		// An entry of the shared root that is a symlink could point the
		// recursive change at the agent's files next to it.
		if sharedEntries[dir] {
			if parent, err := resolveUninstallPath(filepath.Dir(dir)); err != nil || root != filepath.Join(parent, filepath.Base(dir)) {
				return nil, fmt.Errorf("refusing recursive permission changes for %s: it is a symlink to %s next to the agent's files in %s; replace it with what it points to",
					dir, root, sharedApplicationRoot)
			}
		}
		roots = append(roots, root)
	}
	var unique []string
	for n, root := range roots {
		covered := false
		for other, parent := range roots {
			if n != other && pathWithin(parent, root) && (parent != root || other < n) {
				covered = true
				break
			}
		}
		if !covered {
			unique = append(unique, root)
		}
	}
	return unique, nil
}
