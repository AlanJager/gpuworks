//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (s *ClusterRaftSession) installJoinedCatalog(ctx context.Context, cfg Config) error {
	st, err := store.OpenJoinedMemberStore(ctx, s.Database(), bus.New())
	if err != nil {
		return err
	}
	return installVerifiedJoinedCatalog(ctx, cfg, st)
}

// ReceiveClusterCatalog copies the current reference snapshot into the guarded
// join directory. A successful copy is not a readiness or membership decision:
// references can change while copying and require a later coordinated check.
func ReceiveClusterCatalog(ctx context.Context, cfg Config, target ClusterJoinTarget) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.DataDir == "" {
		return errors.New("catalog receipt requires an explicit data directory")
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	database := filepath.Join(root, "gpuworks.db")
	if cfg.DBPath != "" {
		absolute, err := filepath.Abs(cfg.DBPath)
		if err != nil {
			return err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil || parent != root || filepath.Base(absolute) != "gpuworks.db" {
			return errors.New("catalog receipt requires the managed database layout")
		}
	}
	path, release, err := store.LockRuntimeDatabase(database)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := store.CheckRestoreCompleted(root); err != nil {
		return err
	}
	target.Address, err = identity.NormalizeMemberAddress(target.Address)
	if err != nil {
		return err
	}
	info, err := finishClusterJoin(path, target)
	if err != nil {
		return err
	}
	dir := path + ".join"
	key, err := identity.LoadMemberKey(filepath.Join(dir, "member.key"))
	if err != nil {
		return err
	}
	found, err := recoverJoinMaterials(dir, info, key)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("catalog receipt requires committed member materials")
	}
	marker, err := readJoinMaterialFile(filepath.Join(dir, "materials.commit"), 256)
	if err != nil {
		return err
	}
	fields := strings.Split(string(marker), "\n")
	if len(fields) != 3 {
		return errors.New("invalid join material commit identity")
	}
	snapshot, err := loadJoinMaterialSnapshot(filepath.Join(dir, "materials"), info, key, fields[1])
	if err != nil {
		return err
	}
	client, err := enrollment.NewClient(info.EnrollmentURL, info.ClusterID, info.CAFingerprint, info.Protocol)
	if err != nil {
		return err
	}
	defer client.Close()
	return receiveJoinCatalog(ctx, filepath.Join(dir, "catalog-packages"), client, []byte(snapshot.CertificatePEM), key)
}

type joinCatalogSource interface {
	FetchCatalogIndex(context.Context, []byte, []byte) (enrollment.CatalogPackageIndex, error)
	FetchCatalogPackage(context.Context, []byte, []byte, string) (inferencecatalog.Archive, error)
}

func receiveJoinCatalog(ctx context.Context, root string, source joinCatalogSource, certificate, key []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stat, err := os.Lstat(root); err == nil {
		if !stat.IsDir() {
			return errors.New("join catalog root must be a directory and not a link")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	index, err := source.FetchCatalogIndex(ctx, certificate, key)
	if err != nil {
		return err
	}
	cache, err := packages.New(root)
	if err != nil {
		return err
	}
	for _, digest := range index.Digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		archive, err := source.FetchCatalogPackage(ctx, certificate, key, digest)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		got, err := inferencecatalog.PackageDigest(archive.FS)
		if err != nil {
			return err
		}
		if got != digest {
			return errors.New("join catalog package differs from its requested digest")
		}
		if _, _, err := cache.Put(archive.FS); err != nil {
			return err
		}
	}
	// The initial index is only a snapshot. A reference added or removed while
	// copying needs another receipt attempt before this command acknowledges it.
	current, err := source.FetchCatalogIndex(ctx, certificate, key)
	if err != nil {
		return err
	}
	if !slices.Equal(index.Digests, current.Digests) {
		return errors.New("catalog references changed during receipt")
	}
	for _, digest := range current.Digests {
		if _, err := cache.Load(digest); err != nil {
			return err
		}
	}
	return ctx.Err()
}
