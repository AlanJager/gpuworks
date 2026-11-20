package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ClusterTransportPolicyPath follows New's persisted-runtime selection: the
// presence of a joined identity chooses joined transport even after ordinary
// serve starts. It never chooses another policy because the selected one is
// absent, corrupt or revoked, and it grants no readiness or admission authority.
// A non-nil identity is the existing, key-bound join configuration.
func ClusterTransportPolicyPath(cfg Config) (string, *ClusterJoinInfo, error) {
	path := cfg.DBPath
	if path == "" {
		path = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	join := path + ".join"
	stat, err := os.Lstat(join)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(path+".dqlite", ".transport", "policy.json"), nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !stat.IsDir() || stat.Mode().Perm()&0077 != 0 || stat.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("persisted join identity must be a private plain directory")
	}
	if filepath.Clean(path) != filepath.Clean(filepath.Join(cfg.DataDir, "gpuworks.db")) {
		return "", nil, errors.New("joined runtime requires the database path inside its data directory")
	}
	raw, err := readPrivateJoinFile(filepath.Join(join, "config.json"))
	if err != nil {
		return "", nil, err
	}
	var info ClusterJoinInfo
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&info); err != nil {
		return "", nil, errors.New("invalid persisted join configuration")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", nil, errors.New("trailing persisted join configuration")
	}
	if info.ClusterID == "" || info.CAFingerprint == "" || info.Protocol != proto.ClusterProtocolVersion || info.Address == "" {
		return "", nil, errors.New("persisted join configuration is incomplete")
	}
	key, err := identity.LoadMemberKey(filepath.Join(join, "member.key"))
	if err != nil {
		return "", nil, err
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		return "", nil, err
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil || fingerprint != info.KeyFingerprint {
		return "", nil, errors.New("persisted join configuration differs from its member key")
	}
	return filepath.Join(join, "transport-policy.json"), &info, nil
}

// readPrivateJoinFile reads one bounded regular file that only its owner can
// access. A link, an exposed mode or an empty file is refused.
func readPrivateJoinFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || info.Size() > 128<<10 {
		return nil, errors.New("persisted join configuration is missing or exposed")
	}
	return os.ReadFile(path)
}
