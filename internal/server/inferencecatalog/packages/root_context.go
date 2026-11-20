package packages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// PutRootContext publishes through a caller-owned directory capability. The
// caller keeps root open and excludes other package publishers and collectors.
// Cancellation is checked between operations; blocked OS calls must return.
// published records an actual rename, including a later sync/close failure.
func PutRootContext(ctx context.Context, root *os.Root, fsys fs.FS) (digest string, published bool, err error) {
	return PutRootContextWithProgress(ctx, root, fsys, nil)
}

// PutRootContextWithProgress reports only copied, synced files whose owned tree
// has been rechecked. Progress is not publication; final validation, durability
// and rename checks still determine the returned digest and published flag.
func PutRootContextWithProgress(ctx context.Context, root *os.Root, fsys fs.FS, progress func() error) (digest string, published bool, err error) {
	defer func() { err = errors.Join(err, ctx.Err()) }()
	if root == nil || fsys == nil {
		return "", false, errors.New("catalog package root or source is unavailable")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	source := cancellableFS{FS: fsys, ctx: ctx}
	if _, err = inferencecatalog.LoadPackage(source); err != nil {
		return
	}
	if digest, err = inferencecatalog.PackageDigest(source); err != nil {
		return
	}
	if _, statErr := root.Lstat(digest); statErr == nil {
		var child *os.Root
		var info fs.FileInfo
		child, info, err = openPlainPackageRoot(root, digest)
		if err != nil {
			return
		}
		defer func() { err = errors.Join(err, child.Close()) }()
		if err = syncVerifiedRootPackage(ctx, child, digest); err != nil {
			return
		}
		if err = checkPackageRoot(root, digest, child, info); err != nil {
			return
		}
		err = syncRootPackagePath(root, ".")
		return
	} else if !errors.Is(statErr, os.ErrNotExist) {
		err = statErr
		return
	}
	name, child, info, createErr := createPackageStage(root)
	if createErr != nil {
		err = createErr
		return
	}
	directories := map[string]*os.Root{".": child}
	owned := map[string]fs.FileInfo{".": info}
	defer func() {
		if !published {
			err = errors.Join(err, cleanOwnedPackageStage(root, name, child, info, owned, directories))
		}
		for _, directory := range directories {
			err = errors.Join(err, directory.Close())
		}
	}()
	// checkStage is the per-entry guard: the stage is still the directory this
	// call created. checkEntry adds the entry just written and its parent.
	// The whole owned tree is compared before and after publication; doing
	// that for every entry made the copy quadratic in the package's size.
	checkStage := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return checkPackageRoot(root, name, child, info)
	}
	checkEntry := func(entry string) error {
		if e := checkStage(); e != nil {
			return e
		}
		return checkOwnedPackageEntries(child, owned, entry, path.Dir(entry))
	}
	check := func() error {
		if e := checkStage(); e != nil {
			return e
		}
		return checkOwnedPackageTree(child, owned)
	}
	err = fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return check()
		}
		if e := checkStage(); e != nil {
			return e
		}
		if !fs.ValidPath(name) {
			return fs.ErrInvalid
		}
		parent := directories[path.Dir(name)]
		if parent == nil {
			return errors.New("catalog package parent was not captured")
		}
		if entry.IsDir() {
			if e := parent.Mkdir(path.Base(name), 0755); e != nil {
				return e
			}
			dir, identity, e := openPlainPackageRoot(parent, path.Base(name))
			if e != nil {
				return e
			}
			directories[name], owned[name] = dir, identity
			return checkEntry(name)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("catalog package contains non-regular file %s", name)
		}
		data, e := fs.ReadFile(source, name)
		if e != nil {
			return e
		}
		if e = checkStage(); e != nil {
			return e
		}
		file, e := parent.OpenFile(path.Base(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if e != nil {
			return e
		}
		identity, statErr := file.Stat()
		if statErr == nil {
			owned[name] = identity
		}
		written, writeErr := file.Write(data)
		if writeErr == nil && written != len(data) {
			writeErr = io.ErrShortWrite
		}
		identity, afterErr := file.Stat()
		if afterErr == nil {
			owned[name] = identity
		}
		e = errors.Join(statErr, writeErr, afterErr, file.Sync(), file.Close())
		if e != nil {
			return e
		}
		if e = checkEntry(name); e != nil {
			return e
		}
		if progress != nil {
			if e = ctx.Err(); e != nil {
				return e
			}
			return errors.Join(progress(), ctx.Err())
		}
		return nil
	})
	if err != nil {
		return
	}
	if _, err = verifyRootPackage(ctx, child, digest); err != nil {
		return
	}
	if err = syncRootPackage(ctx, child); err != nil {
		return
	}
	if err = check(); err != nil {
		return
	}
	published, err = renameRootPackageNoReplace(root, name, digest)
	if err != nil {
		if published {
			return
		}
		// Reuse only a complete immutable package installed by another publisher.
		if !errors.Is(err, fs.ErrExist) {
			return
		}
		var existing *os.Root
		var existingInfo fs.FileInfo
		existing, existingInfo, err = openPlainPackageRoot(root, digest)
		if err != nil {
			return
		}
		defer func() { err = errors.Join(err, existing.Close()) }()
		if err = syncVerifiedRootPackage(ctx, existing, digest); err != nil {
			return
		}
		if err = checkPackageRoot(root, digest, existing, existingInfo); err != nil {
			return
		}
		err = syncRootPackagePath(root, ".")
		return
	}
	published = true
	if err = checkPackageRoot(root, digest, child, info); err != nil {
		return
	}
	if err = checkOwnedPackageTree(child, owned); err != nil {
		return
	}
	err = syncRootPackagePath(root, ".")
	return
}

// LoadRootContext only reads a plain immutable digest beneath the caller's root.
// The returned projection retains no child root descriptor.
func LoadRootContext(ctx context.Context, root *os.Root, digest string) (pkg inferencecatalog.Package, err error) {
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			pkg = inferencecatalog.Package{}
		}
	}()
	if root == nil || !digestPattern.MatchString(digest) {
		return pkg, errors.New("invalid catalog package root or digest")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	child, info, err := openPlainPackageRoot(root, digest)
	if err != nil {
		return pkg, err
	}
	defer func() {
		err = errors.Join(err, child.Close())
		if err != nil {
			pkg = inferencecatalog.Package{}
		}
	}()
	pkg, err = ValidateRootContext(ctx, child, digest)
	if err != nil {
		return pkg, err
	}
	if err = checkPackageRoot(root, digest, child, info); err != nil {
		return pkg, err
	}
	return pkg, nil
}

// ValidateRootContext verifies an expected digest through a caller-held package
// directory. It reads only that root, retains no descriptors, and does not close
// the caller's root. The caller checks its parent namespace and serializes writes.
func ValidateRootContext(ctx context.Context, root *os.Root, digest string) (pkg inferencecatalog.Package, err error) {
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			pkg = inferencecatalog.Package{}
		}
	}()
	if root == nil || !digestPattern.MatchString(digest) {
		return pkg, errors.New("invalid catalog package root or digest")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	identities, err := snapshotPlainPackageTree(root)
	if err != nil {
		return pkg, err
	}
	pkg, err = verifyRootPackage(ctx, root, digest)
	if err != nil {
		return pkg, err
	}
	err = checkOwnedPackageTree(root, identities)
	return
}

func createPackageStage(root *os.Root) (string, *os.Root, fs.FileInfo, error) {
	for range 8 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, nil, err
		}
		name := ".package-" + hex.EncodeToString(random[:])
		if err := root.Mkdir(name, 0700); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, nil, err
		}
		child, info, err := openPlainPackageRoot(root, name)
		if err != nil {
			return "", nil, nil, err
		}
		return name, child, info, nil
	}
	return "", nil, nil, errors.New("catalog package staging collision limit exceeded")
}
