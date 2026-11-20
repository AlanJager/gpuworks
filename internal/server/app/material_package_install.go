//go:build linux && dqlite

package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

func installReceivedPackage(ctx context.Context, source, target *os.Root, dataDir, digest string, files []materialManifestFile) (result error) {
	return installReceivedPackageWithProgress(ctx, source, target, dataDir, digest, files, nil)
}

func installReceivedPackageWithProgress(ctx context.Context, source, target *os.Root, dataDir, digest string, files []materialManifestFile, progress func() error) (result error) {
	if err := checkMaterialDataRoot(dataDir, target); err != nil {
		return err
	}
	if err := ensureMaterialParent(target, "catalog-packages/"+digest); err != nil {
		return err
	}
	catalog, err := openPlainMaterialDirectory(target, "catalog-packages")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, catalog.Close()) }()
	original, err := catalog.Stat(".")
	if err != nil {
		return err
	}
	check := func() error {
		return errors.Join(ctx.Err(), checkMaterialDataRoot(dataDir, target), checkMaterialOwnedDirectory(target, "catalog-packages", original))
	}
	if err := check(); err != nil {
		return err
	}
	if _, err := catalog.Lstat(digest); err == nil {
		_, err = packages.LoadRootContext(ctx, catalog, digest)
		return errors.Join(err, check())
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	name, stage, identity, err := createReceivedPackageStage(catalog)
	if err != nil {
		return err
	}
	owned := map[string]fs.FileInfo{".": identity}
	defer func() {
		result = errors.Join(result, removeOwnedReceivedPackageStage(catalog, name, stage, identity, owned), stage.Close())
	}()
	for _, file := range files {
		if err := check(); err != nil {
			return err
		}
		rel := strings.TrimPrefix(file.Path, "catalog-packages/"+digest+"/")
		if rel == file.Path || rel == "" || !validMaterialConsumerPath(file.Class, file.Path) {
			return errors.New("catalog file escaped its package")
		}
		if err := prepareReceivedPackageParents(stage, rel, owned); err != nil {
			return err
		}
		if err := copyReceivedMaterial(ctx, source, stage, rel, file.SHA256, file.Bytes, false); err != nil {
			return err
		}
		info, err := stage.Lstat(rel)
		if err != nil {
			return err
		}
		owned[rel] = info
		if progress != nil {
			if err := check(); err != nil {
				return err
			}
			if err := completedMaterialInstallationStage(ctx, progress); err != nil {
				return err
			}
		}
	}
	if err := checkOwnedReceivedPackageTree(stage, owned); err != nil {
		return err
	}
	// Refuse a different complete package before publishing any digest. All
	// reads stay on the same captured stage, including package-store copying.
	if _, err := packages.ValidateRootContext(ctx, stage, digest); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := completedMaterialInstallationStage(ctx, progress); err != nil {
		return err
	}
	actual, _, err := packages.PutRootContextWithProgress(ctx, catalog, stage.FS(), progress)
	if err != nil {
		return err
	}
	if actual != digest {
		return errors.New("received package changed before publication")
	}
	if err := check(); err != nil {
		return err
	}
	if err := completedMaterialInstallationStage(ctx, progress); err != nil {
		return err
	}
	_, err = packages.LoadRootContext(ctx, catalog, digest)
	return errors.Join(err, check())
}

func checkMaterialDataRoot(dataDir string, root *os.Root) error {
	named, err := os.Lstat(dataDir)
	if err != nil {
		return err
	}
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(named, held) {
		return errors.New("material consumer data root identity changed")
	}
	return nil
}

func createReceivedPackageStage(root *os.Root) (string, *os.Root, fs.FileInfo, error) {
	for range 8 {
		var nonce [4]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, nil, err
		}
		name := fmt.Sprintf(".material-package-%d", binary.LittleEndian.Uint32(nonce[:]))
		if err := root.Mkdir(name, 0700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, nil, err
		}
		child, err := openPlainMaterialDirectory(root, name)
		if err != nil {
			return "", nil, nil, err
		}
		info, err := child.Stat(".")
		if err != nil {
			return "", nil, nil, errors.Join(err, child.Close())
		}
		return name, child, info, nil
	}
	return "", nil, nil, errors.New("received package stage collision limit exceeded")
}

func prepareReceivedPackageParents(root *os.Root, name string, owned map[string]fs.FileInfo) error {
	current := "."
	for _, part := range strings.Split(path.Dir(name), "/") {
		if part == "." {
			continue
		}
		current = path.Join(current, part)
		if before, exists := owned[current]; exists {
			if err := checkMaterialOwnedDirectory(root, current, before); err != nil {
				return err
			}
			continue
		}
		// New directories must be ours; an already-existing unexpected name is not
		// admitted as cleanup authority even if its contents happen to be valid.
		if err := root.Mkdir(current, 0700); err != nil {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("received package directory changed during creation")
		}
		owned[current] = info
	}
	return nil
}

func checkOwnedReceivedPackageTree(root *os.Root, owned map[string]fs.FileInfo) error {
	count := 0
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		before := owned[name]
		if before == nil {
			return fmt.Errorf("received package contains an unowned entry: %s", name)
		}
		after, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) || before.Mode() != after.Mode() || (!after.IsDir() && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()))) {
			return fmt.Errorf("received package entry identity changed: %s", name)
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count != len(owned) {
		return errors.New("received package owned entries disappeared")
	}
	return nil
}

func removeOwnedReceivedPackageStage(parent *os.Root, name string, stage *os.Root, original fs.FileInfo, owned map[string]fs.FileInfo) error {
	if err := checkMaterialOwnedDirectory(parent, name, original); err != nil {
		return err
	}
	if err := checkOwnedReceivedPackageTree(stage, owned); err != nil {
		return err
	}
	names := make([]string, 0, len(owned))
	for name := range owned {
		if name != "." {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		before, err := stage.Lstat(name)
		if err != nil {
			return err
		}
		if !os.SameFile(before, owned[name]) {
			return errors.New("received package cleanup entry identity changed")
		}
		if err := stage.Remove(name); err != nil {
			return err
		}
	}
	if err := checkMaterialOwnedDirectory(parent, name, original); err != nil {
		return err
	}
	return errors.Join(parent.Remove(name), syncRootDir(parent, "."))
}
