//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ReceiveClusterTransportPolicy refreshes a guarded joining member's durable
// authorization snapshot. Receipt never grants membership or application readiness.
func ReceiveClusterTransportPolicy(ctx context.Context, cfg Config, target ClusterJoinTarget) error {
	return withClusterJoinMaterials(ctx, cfg, target, func(dir string, info ClusterJoinInfo, key []byte, snapshot joinMaterialSnapshot) error {
		client, err := enrollment.NewClient(info.EnrollmentURL, info.ClusterID, info.CAFingerprint, info.Protocol)
		if err != nil {
			return err
		}
		defer client.Close()
		source, err := client.FetchTransportPolicy(ctx, []byte(snapshot.CertificatePEM), key)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err = identity.ApplyTransportPolicySnapshot(filepath.Join(dir, "transport-policy.json"), identity.TransportPolicy{
			ClusterID: source.ClusterID, CAFingerprint: source.CAFingerprint,
			SourceRevision: source.SourceRevision, AllowedKeys: source.AllowedKeys, RevokedKeys: source.RevokedKeys,
		})
		return err
	})
}

type clusterJoinHold struct {
	path     string
	dir      string
	info     ClusterJoinInfo
	key      []byte
	snapshot joinMaterialSnapshot
	release  func() error
}

// lockClusterJoinMaterials holds the runtime lease and the existing join gate.
// The caller must invoke release, including when later startup fails.
func lockClusterJoinMaterials(ctx context.Context, cfg Config, target ClusterJoinTarget) (_ *clusterJoinHold, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		return nil, errors.New("transport policy receipt requires an explicit data directory")
	}
	root, err := filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	database := filepath.Join(root, "gpuworks.db")
	if cfg.DBPath != "" {
		absolute, err := filepath.Abs(cfg.DBPath)
		if err != nil {
			return nil, err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
		if err != nil || parent != root || filepath.Base(absolute) != "gpuworks.db" {
			return nil, errors.New("transport policy receipt requires the managed database layout")
		}
	}
	path, release, err := store.LockRuntimeDatabase(database)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, release())
		}
	}()
	if err := store.CheckRestoreCompleted(root); err != nil {
		return nil, err
	}
	target.Address, err = identity.NormalizeMemberAddress(target.Address)
	if err != nil {
		return nil, err
	}
	info, err := finishClusterJoin(path, target)
	if err != nil {
		return nil, err
	}
	dir := path + ".join"
	key, err := identity.LoadMemberKey(filepath.Join(dir, "member.key"))
	if err != nil {
		return nil, err
	}
	found, err := recoverJoinMaterials(dir, info, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("transport policy receipt requires committed member materials")
	}
	marker, err := readJoinMaterialFile(filepath.Join(dir, "materials.commit"), 256)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(marker), "\n")
	if len(fields) != 3 {
		return nil, errors.New("invalid join material commit identity")
	}
	snapshot, err := loadJoinMaterialSnapshot(filepath.Join(dir, "materials"), info, key, fields[1])
	if err != nil {
		return nil, err
	}
	return &clusterJoinHold{path: path, dir: dir, info: info, key: key, snapshot: snapshot, release: release}, nil
}

// withClusterJoinMaterials keeps the runtime lease across the entire callback.
func withClusterJoinMaterials(ctx context.Context, cfg Config, target ClusterJoinTarget, use func(string, ClusterJoinInfo, []byte, joinMaterialSnapshot) error) (resultErr error) {
	hold, err := lockClusterJoinMaterials(ctx, cfg, target)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, hold.release()) }()
	return use(hold.dir, hold.info, hold.key, hold.snapshot)
}
