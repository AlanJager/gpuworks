//go:build linux && dqlite

package store

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// OpenSparseJoinedRaftCandidate holds the existing member's runtime lease while
// awaiting an independently authenticated live membership proof. It starts no
// native process, performs no networking and grants no role or readiness.
func OpenSparseJoinedRaftCandidate(ctx context.Context, path, address, clusterID, pin string, ca, certificate, key []byte) (_ *LegacyJoinedRaftSession, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normalized, err := identity.NormalizeMemberAddress(address)
	if err != nil || normalized != address {
		return nil, errors.Join(ErrClusterIdentityConflict, err)
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
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(errors.New("sparse joined candidate must not contain a SQLite database"), err)
	}
	dir := path + ".dqlite"
	if err := checkSparseJoinedFiles(dir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("run sparse joined candidate as its persisted directory owner")
	}
	fingerprint, err := persistedLegacyJoinIdentity(path, address, clusterID, pin, key)
	if err != nil {
		return nil, err
	}
	if err := requireLegacyJoinGuard(path+".maintenance", fingerprint); err != nil {
		return nil, err
	}
	id, _, err := cluster.PublishedJoinSeeds(ctx, dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "info.yaml"))
	if err != nil {
		return nil, err
	}
	var persisted client.NodeInfo
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&persisted); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("sparse joined identity has trailing YAML")
	}
	if persisted.ID != id || persisted.Address != address {
		return nil, ErrClusterIdentityConflict
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	policy, err := identity.LoadTransportPolicy(filepath.Join(path+".join", "transport-policy.json"))
	if err != nil {
		return nil, err
	}
	authorize, err := policy.Authorizer(clusterID, pin)
	if err != nil {
		return nil, err
	}
	if err := authorize(fingerprint); err != nil {
		return nil, err
	}
	listen, _, err := trust.TLSConfigs(certificate, key, time.Now().UTC(), authorize)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(listen.Certificates[0].Certificate[0])
	if err != nil {
		return nil, err
	}
	verified, err := trust.VerifyMember(leaf, time.Now().UTC())
	if err != nil || verified != fingerprint {
		return nil, errors.Join(ErrClusterIdentityConflict, err)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if err := leaf.VerifyHostname(host); err != nil {
		return nil, fmt.Errorf("sparse joined certificate address: %w", err)
	}
	if err := checkSparseJoinedFiles(dir); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &LegacyJoinedRaftSession{id: id, address: address, raftDir: dir, release: release}, nil
}

// AwaitingMembershipProof identifies a lease-only candidate. It is not proof
// that the member is currently a spare, eligible to start, or still admitted.
func (s *LegacyJoinedRaftSession) AwaitingMembershipProof() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db == nil && s.id != 0 && s.release != nil
}
