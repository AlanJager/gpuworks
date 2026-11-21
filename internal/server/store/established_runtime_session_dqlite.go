//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// StartEstablishedRaft resumes the original configured transport and Raft
// process without waiting for quorum. OpenClassifiedStore later qualifies one
// live SQL snapshot before transferring the same process and runtime lease.
// No schema, admission, maintenance or join marker is created here.
func StartEstablishedRaft(ctx context.Context, path string, expected ClusterIdentity, expectedKey string) (_ *LegacyJoinedRaftSession, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion || !validClusterDigest(expectedKey, 32) {
		return nil, ErrClusterIdentityConflict
	}
	path, release, err := LockRuntimeDatabase(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, release())
		}
	}()
	if err := CheckRestoreCompleted(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("existing SQLite database %q requires explicit import", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Lstat(path + ".join"); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(ErrClusterIdentityConflict, err, errors.New("established seed must not have a joined identity directory"))
	}
	raftDir := path + ".dqlite"
	if err := checkExistingNativeFiles(raftDir); err != nil {
		return nil, err
	}
	info, err := os.Stat(raftDir)
	if err != nil {
		return nil, err
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("run established Raft as its persisted directory owner")
	}
	db, err := cluster.StartEstablishedMemberDatabase(ctx, raftDir, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	clusterID, pin := db.TransportIdentity()
	if clusterID != expected.ClusterID || pin != expected.CAFingerprint || db.TransportMemberKey() != expectedKey || db.ID == 0 || db.Address == "" {
		return nil, errors.Join(ErrClusterIdentityConflict, db.Close())
	}
	if _, err := os.Lstat(path + ".join"); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(ErrClusterIdentityConflict, err, errors.New("joined identity appeared during established startup"), db.Close())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &LegacyJoinedRaftSession{db: db, id: db.ID, address: db.Address, raftDir: raftDir, release: release}, nil
}

// Both exact schema versions must qualify the live transport key against its
// current admission before ownership transfers. A legacy marker is not a seed
// or joined identity exemption; the local identity directory and replicated
// invitation role must agree in this same read-only transaction.
func classifyEstablishedRuntimeSnapshot(ctx context.Context, tx *gorm.DB, expected ClusterIdentity, key, address, raftDir string) (bool, error) {
	class, err := classifyJoinedSchemaClass(ctx, tx, expected)
	if err != nil {
		return false, err
	}
	legacy := class == joinedSchemaLegacy
	if !validClusterDigest(key, 32) || address == "" || !strings.HasSuffix(raftDir, ".dqlite") {
		return false, ErrClusterIdentityConflict
	}
	joinPath := strings.TrimSuffix(raftDir, ".dqlite") + ".join"
	joinInfo, err := os.Lstat(joinPath)
	joined := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.Join(ErrClusterIdentityConflict, err)
	}
	if joined && (!joinInfo.IsDir() || joinInfo.Mode()&os.ModeSymlink != 0 || joinInfo.Mode().Perm()&0077 != 0) {
		return false, ErrClusterIdentityConflict
	}
	var admission ClusterAdmission
	if err := tx.Where("key_fingerprint = ?", key).First(&admission).Error; err != nil {
		return false, errors.Join(ErrClusterAdmissionRejected, err)
	}
	if admission.KeyFingerprint != key || admission.Address != address || admission.RevokedAt != nil ||
		(joined && admission.InvitationHash == "") || (!joined && admission.InvitationHash != "") {
		return false, ErrClusterAdmissionRejected
	}
	current, statErr := os.Lstat(joinPath)
	if joined {
		if statErr != nil || !os.SameFile(joinInfo, current) || joinInfo.Mode() != current.Mode() {
			return false, errors.Join(ErrClusterIdentityConflict, statErr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, errors.Join(ErrClusterIdentityConflict, statErr)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if class == joinedSchemaPending {
		// The member qualified; only its executable is ahead of the schema.
		return false, ErrSchemaUpgradePending
	}
	return legacy, nil
}
