//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func startWaitingJoinedRaft(ctx context.Context, cfg Config, bundle *enrollmentBundle) (joinedRaftSession, error) {
	target, err := persistedClusterJoinTarget(cfg)
	if err != nil {
		return nil, err
	}
	joinDir := cfg.DBPath + ".join"
	if cfg.DBPath == "" {
		joinDir = filepath.Join(cfg.DataDir, "gpuworks.db.join")
	}
	info, err := loadClusterJoin(joinDir, target)
	if err != nil {
		return nil, err
	}
	materialDir := filepath.Join(cfg.DataDir, enrollmentDirectory)
	certificate, err := readJoinMaterialFile(filepath.Join(materialDir, "server.pem"), 128<<10)
	if err != nil {
		return nil, err
	}
	key, err := readJoinMaterialFile(filepath.Join(materialDir, "server.key"), 128<<10)
	if err != nil {
		return nil, err
	}
	path := cfg.DBPath
	if path == "" {
		path = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	session, startErr := store.StartLegacyJoinedRaft(ctx, path, info.Address, bundle.info.ClusterID, bundle.info.CAFingerprint, bundle.ca, certificate, key)
	if startErr == nil {
		return session, nil
	}
	// Sparse identities retain only a lease and health listener. Cached role
	// data cannot authorize starting Raft; that requires a separate live proof.
	candidate, err := store.OpenSparseJoinedRaftCandidate(ctx, path, info.Address, bundle.info.ClusterID, bundle.info.CAFingerprint, bundle.ca, certificate, key)
	if err != nil {
		return nil, errors.Join(startErr, err)
	}
	return candidate, nil
}
