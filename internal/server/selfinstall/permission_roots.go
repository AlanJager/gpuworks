package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	config := l.ConfigPath()
	if migrated, _ := l.sharedConfig(); migrated {
		config = l.sharedConfigDir()
	}
	for _, path := range []string{l.Binary(), config, filepath.Join(l.Dir, installJSONFile),
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
	if _, err := i.layout.sharedConfig(); err != nil {
		return nil, err
	}
	dirs := []string{i.layout.Dir, i.layout.DataDir, i.layout.LogsDir}
	sharedEntries := map[string]bool{}
	if i.installedInSharedRoot() {
		// The binary stays root's here (see rootOwnSharedBinary).
		var entries []string
		for _, entry := range i.layout.serverEntries() {
			if entry != i.layout.Binary() {
				entries = append(entries, entry)
				sharedEntries[entry] = true
			}
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
	if i.installedInSharedRoot() {
		if err := i.checkSharedRootEntries(); err != nil {
			return nil, err
		}
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

// checkSharedRootEntries inspects the shared root's direct entries that a
// privileged recursive chown or chmod (or the configuration move) is about to
// act on. An older installer let the service account own the root, so it may
// have replaced any of them: a symlink given on the command line is followed
// by chmod -R, and a hard link makes chown change the linked file. Refused
// are symlinks the operator did not create (a data or logs directory may be a
// root-owned link to another disk), unexpected file types, and regular files
// with more than one link.
func (i *Installer) checkSharedRootEntries() error {
	dirs := map[string]bool{i.layout.sharedConfigDir(): true, i.layout.TmpDir(): true, i.layout.BackupsDir: true,
		i.layout.DataDir: true, i.layout.LogsDir: true}
	linkable := map[string]bool{i.layout.DataDir: true, i.layout.LogsDir: true}
	paths := []string{i.layout.Binary(), filepath.Join(i.layout.Dir, installJSONFile), i.layout.PIDFile(), i.layout.LockPath()}
	if migrated, _ := i.layout.sharedConfig(); !migrated {
		paths = append(paths, i.layout.ConfigPath())
	}
	for dir := range dirs {
		paths = append(paths, dir)
	}
	for _, path := range paths {
		path = filepath.Clean(path)
		if filepath.Dir(path) != filepath.Clean(i.layout.Dir) {
			continue
		}
		info, err := lstatOwned(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if linkable[path] && trustedWriteOwner(info) {
				continue
			}
			return fmt.Errorf("refusing permission changes in %s: %s is a symlink that root did not create; check where it points and replace it with that directory, or recreate the link as root",
				i.layout.Dir, path)
		case dirs[path]:
			if !info.IsDir() {
				return fmt.Errorf("refusing permission changes in %s: %s is not a directory", i.layout.Dir, path)
			}
		case !info.Mode().IsRegular():
			return fmt.Errorf("refusing permission changes in %s: %s is not a regular file", i.layout.Dir, path)
		case linkCount(info) > 1:
			return fmt.Errorf("refusing permission changes in %s: %s has %d hard links, so changing it would change another file too; replace it with a copy",
				i.layout.Dir, path, linkCount(info))
		}
	}
	return nil
}

// linkCount is a file's hard link count, or 1 where the platform hides it.
func linkCount(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}

// rootOwnSharedBinary keeps the binary in the shared application root owned
// by root with mode 0755. Root runs it (sudo gpuworks upgrade, and supervisor
// hooks that run before the service as root), so a service account that could
// rewrite it in place would become root. Elsewhere the binary belongs to the
// service account with the rest of the dedicated tree.
func (i *Installer) rootOwnSharedBinary(ctx context.Context) error {
	if !i.installedInSharedRoot() {
		return nil
	}
	bin := i.layout.Binary()
	info, err := os.Lstat(bin)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || linkCount(info) > 1 {
		return fmt.Errorf("installed binary %s must be a regular file with a single link", bin)
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "chown", Args: []string{"-h", "0:0", bin}, Sudo: true}); err != nil {
		return err
	}
	return i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", binaryMode), bin}, Sudo: true})
}
