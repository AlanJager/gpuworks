//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PrepareClusterJoin publishes the member key and expected target before any
// credential exchange. The same runtime lease protects both this operation and
// normal startup. A published .join directory blocks standalone startup until
// a later authorized coordinator completes membership and material installation.
func PrepareClusterJoin(ctx context.Context, cfg Config, target ClusterJoinTarget) (result ClusterJoinInfo, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	address, err := identity.NormalizeMemberAddress(target.Address)
	if err != nil {
		return result, err
	}
	target.Address = address
	if target.EnrollmentOrigin != "" && !enrollment.CanonicalEnrollmentOrigin(target.EnrollmentOrigin) {
		return result, errors.New("join member public origin must be canonical HTTPS")
	}
	if (target.EnrollmentOrigin == "") != (target.EnrollmentListen == "") {
		return result, errors.New("join public origin and local HTTPS listen address must be set together")
	}
	if target.EnrollmentOrigin != "" {
		if _, err := validateEnrollmentAddresses(target.EnrollmentListen, target.EnrollmentOrigin); err != nil {
			return result, err
		}
	}
	client, err := enrollment.NewClient(target.EnrollmentURL, target.ClusterID, target.CAFingerprint, target.Protocol)
	if err != nil {
		return result, err
	}
	client.Close()
	if cfg.DataDir == "" {
		return result, errors.New("join requires an explicit data directory")
	}
	if err := store.CheckRestoreCompleted(cfg.DataDir); err != nil {
		return result, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return result, err
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return result, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return result, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return result, errors.New("prepare cluster join as the data directory owner")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(root, "gpuworks.db")
	}
	absoluteDB, err := filepath.Abs(cfg.DBPath)
	if err != nil {
		return result, err
	}
	databaseParent, err := filepath.EvalSymlinks(filepath.Dir(absoluteDB))
	if err != nil || databaseParent != root || filepath.Base(absoluteDB) != "gpuworks.db" {
		return result, errors.New("cluster join currently requires the managed gpuworks.db layout inside its data directory")
	}
	path, release, err := store.LockRuntimeDatabase(cfg.DBPath)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if filepath.Dir(path) != root {
		return result, errors.New("joining database must be inside its data directory")
	}
	if err := store.CheckRestoreCompleted(root); err != nil {
		return result, err
	}
	dir := path + ".join"
	if join, err := os.Lstat(dir); err == nil {
		if !join.IsDir() || join.Mode()&os.ModeSymlink != 0 {
			return result, errors.New("published join identity must be a directory and not a link")
		}
		// Once the identity is published, retries may contain the joined
		// database and received materials. Validate the original key and target
		// before touching either; the empty-directory rule applies only to a
		// first join.
		return finishClusterJoin(path, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return result, err
	}
	base := filepath.Base(path)
	for _, entry := range entries {
		name := entry.Name()
		if name != base+".runtime.lock" && name != base+".join" && name != base+".maintenance" && !strings.HasPrefix(name, ".cluster-join-") {
			return result, errors.New("join requires an unused data directory; existing database, materials or maintenance state must not be overwritten")
		}
	}
	if _, err := os.Lstat(path + ".maintenance"); err == nil {
		return result, errors.New("maintenance state exists without its join identity; restore the original identity instead of generating a replacement")
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	stage, err := os.MkdirTemp(root, ".cluster-join-")
	if err != nil {
		return result, err
	}
	// Unpublished candidates cannot have contacted a peer and may be retained
	// for diagnosis. Once published, a missing or damaged key is never repaired.
	key, err := identity.CreateMemberKey(filepath.Join(stage, "member.key"))
	if err != nil {
		return result, err
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return result, err
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		return result, err
	}
	result = ClusterJoinInfo{ClusterJoinTarget: target, KeyFingerprint: fingerprint}
	config, err := json.Marshal(result)
	if err != nil {
		return ClusterJoinInfo{}, err
	}
	if err := writeEnrollmentFile(filepath.Join(stage, "config.json"), config); err != nil {
		return ClusterJoinInfo{}, err
	}
	if err := syncEnrollmentDirectory(stage); err != nil {
		return ClusterJoinInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ClusterJoinInfo{}, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dir, unix.RENAME_NOREPLACE); err != nil {
		return ClusterJoinInfo{}, err
	}
	if err := syncEnrollmentDirectory(root); err != nil {
		return ClusterJoinInfo{}, err
	}
	return finishClusterJoin(path, target)
}

// The separate marker prevents a missing published identity directory from
// being mistaken for a fresh target after credential exchange has begun.
func finishClusterJoin(path string, target ClusterJoinTarget) (ClusterJoinInfo, error) {
	info, err := loadClusterJoin(path+".join", target)
	if err != nil {
		return ClusterJoinInfo{}, err
	}
	marker := path + ".maintenance"
	expected := []byte("cluster-join " + info.KeyFingerprint + "\n")
	if stat, err := os.Lstat(marker); err == nil {
		if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() != int64(len(expected)) {
			return ClusterJoinInfo{}, errors.New("existing maintenance marker is not this join operation")
		}
		actual, err := os.ReadFile(marker)
		if err != nil || !bytes.Equal(actual, expected) {
			return ClusterJoinInfo{}, errors.New("maintenance marker differs from the persisted join identity")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := writeEnrollmentFile(marker, expected); err != nil {
			return ClusterJoinInfo{}, err
		}
	} else {
		return ClusterJoinInfo{}, err
	}
	if err := syncEnrollmentDirectory(filepath.Dir(path)); err != nil {
		return ClusterJoinInfo{}, err
	}
	return info, nil
}

func loadClusterJoin(dir string, target ClusterJoinTarget) (ClusterJoinInfo, error) {
	var result ClusterJoinInfo
	stat, err := os.Lstat(dir)
	if err != nil {
		return result, err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 || stat.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("join identity must be a private plain directory")
	}
	key, err := identity.LoadMemberKey(filepath.Join(dir, "member.key"))
	if err != nil {
		return result, err
	}
	path := filepath.Join(dir, "config.json")
	info, err := os.Lstat(path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 128<<10 {
		return result, errors.New("join configuration must be a private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ClusterJoinInfo{}, errors.New("invalid join configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ClusterJoinInfo{}, errors.New("trailing join configuration")
	}
	if result.ClusterJoinTarget != target {
		return ClusterJoinInfo{}, errors.New("join destination differs from the persisted target")
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return ClusterJoinInfo{}, err
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil || result.KeyFingerprint != fingerprint {
		return ClusterJoinInfo{}, errors.New("join private key differs from its persisted identity")
	}
	return result, nil
}
