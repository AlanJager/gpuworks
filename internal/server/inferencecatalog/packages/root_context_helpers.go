package packages

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func openPlainPackageRoot(parent *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("catalog package directory is not plain")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	if err := checkPackageRoot(parent, name, child, info); err != nil {
		return nil, nil, errors.Join(err, child.Close())
	}
	return child, info, nil
}

func checkPackageRoot(parent *os.Root, name string, child *os.Root, expected fs.FileInfo) error {
	current, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	held, err := child.Stat(".")
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, expected) || !os.SameFile(held, expected) {
		return fmt.Errorf("catalog package directory ownership changed: %s", name)
	}
	return nil
}

func samePackageEntry(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && (a.IsDir() || (a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())))
}

func snapshotPlainPackageTree(root *os.Root) (map[string]fs.FileInfo, error) {
	infos := map[string]fs.FileInfo{}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("catalog package contains non-regular entry %s", name)
		}
		infos[name] = info
		return nil
	})
	return infos, err
}

func checkOwnedPackageTree(root *os.Root, expected map[string]fs.FileInfo) error {
	actual, err := snapshotPlainPackageTree(root)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("catalog package tree entries changed")
	}
	for name, before := range expected {
		after := actual[name]
		if after == nil || !samePackageEntry(before, after) {
			return fmt.Errorf("catalog package tree ownership changed: %s", name)
		}
	}
	return nil
}

// checkOwnedPackageEntries compares only the named entries with what this
// publisher recorded for them. It is the per-entry guard during a copy; the
// whole tree is still compared before and after publication.
func checkOwnedPackageEntries(root *os.Root, expected map[string]fs.FileInfo, names ...string) error {
	for _, name := range names {
		before := expected[name]
		if before == nil {
			return fmt.Errorf("catalog package entry was not captured: %s", name)
		}
		after, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !samePackageEntry(before, after) {
			return fmt.Errorf("catalog package tree ownership changed: %s", name)
		}
	}
	return nil
}

func cleanOwnedPackageStage(parent *os.Root, name string, child *os.Root, info fs.FileInfo, owned map[string]fs.FileInfo, directories map[string]*os.Root) error {
	if err := checkPackageRoot(parent, name, child, info); err != nil {
		return err
	}
	if err := checkOwnedPackageTree(child, owned); err != nil {
		return err
	}
	names := make([]string, 0, len(owned))
	for name := range owned {
		if name != "." {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] > names[j] })
	for _, leaf := range names {
		// Never recursively delete a replaced directory or unrecorded child.
		directory := directories[path.Dir(leaf)]
		current, err := directory.Lstat(path.Base(leaf))
		if err != nil {
			return err
		}
		if !samePackageEntry(current, owned[leaf]) {
			return fmt.Errorf("catalog package cleanup ownership changed: %s", leaf)
		}
		if err := directory.Remove(path.Base(leaf)); err != nil {
			return err
		}
	}
	if err := checkPackageRoot(parent, name, child, info); err != nil {
		return err
	}
	if err := parent.Remove(name); err != nil {
		return err
	}
	return syncRootPackagePath(parent, ".")
}

func verifyRootPackage(ctx context.Context, root *os.Root, digest string) (inferencecatalog.Package, error) {
	input := cancellableFS{FS: root.FS(), ctx: ctx}
	// One pass validates and digests. The projection is the one the package
	// store loads: a stored official package is digested as published and may
	// hold specs this build skips.
	pkg, actual, err := inferencecatalog.LoadPackageDigest(input)
	if err != nil {
		return inferencecatalog.Package{}, err
	}
	if actual != digest {
		return inferencecatalog.Package{}, fmt.Errorf("catalog package %s content digest is %s", digest, actual)
	}
	return pkg, nil
}

func syncRootPackage(ctx context.Context, root *os.Root) error {
	var dirs []string
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("catalog package contains a non-regular file")
		}
		return syncRootPackagePath(root, name)
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncRootPackagePath(root, dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncRootPackagePath(root *os.Root, name string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// Capture temporary file facts across validation and durability operations.
func syncVerifiedRootPackage(ctx context.Context, root *os.Root, digest string) error {
	identities, err := snapshotPlainPackageTree(root)
	if err != nil {
		return err
	}
	if _, err := verifyRootPackage(ctx, root, digest); err != nil {
		return err
	}
	if err := checkOwnedPackageTree(root, identities); err != nil {
		return err
	}
	if err := syncRootPackage(ctx, root); err != nil {
		return err
	}
	return checkOwnedPackageTree(root, identities)
}
