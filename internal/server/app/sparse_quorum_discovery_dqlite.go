//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The caller retains its original sparse runtime lease through this operation.
// This collects an authenticated observation; it cannot start a native process
// or confer all-voter closure, a spare role, or business readiness.
func discoverSparseQuorumFacts(parent context.Context, cfg Config, bundle *enrollmentBundle) (facts enrollment.ClusterQuorumFacts, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	defer func() {
		result = errors.Join(result, ctx.Err())
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 0 {
			result = errors.Join(result, context.DeadlineExceeded)
		}
		if result != nil {
			facts = enrollment.ClusterQuorumFacts{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return facts, err
	}
	if bundle == nil || bundle.trust == nil {
		return facts, errors.New("sparse discovery requires its persisted enrollment identity")
	}
	if err := verifyPersistedJoinedEnrollment(cfg, bundle); err != nil {
		return facts, err
	}
	target, err := persistedClusterJoinTarget(cfg)
	if err != nil {
		return facts, err
	}
	path := cfg.DBPath
	if path == "" {
		path = filepath.Join(cfg.DataDir, "gpuworks.db")
	}
	info, err := loadClusterJoin(path+".join", target)
	if err != nil {
		return facts, err
	}
	_, seeds, err := cluster.PublishedJoinSeeds(ctx, path+".dqlite")
	if err != nil {
		return facts, err
	}
	policy, err := identity.LoadTransportPolicy(filepath.Join(path+".join", "transport-policy.json"))
	if err != nil {
		return facts, err
	}
	authorize, err := bundle.trust.PolicyAuthorizer(policy)
	if err != nil {
		return facts, err
	}
	if err := authorize(info.KeyFingerprint); err != nil {
		return facts, err
	}
	materialDir := filepath.Join(cfg.DataDir, enrollmentDirectory)
	certificate, err := readJoinMaterialFile(filepath.Join(materialDir, "server.pem"), 128<<10)
	if err != nil {
		return facts, err
	}
	key, err := readJoinMaterialFile(filepath.Join(materialDir, "server.key"), 128<<10)
	if err != nil {
		return facts, err
	}
	_, config, err := bundle.trust.TLSConfigs(certificate, key, time.Now().UTC(), authorize)
	if err != nil {
		return facts, err
	}
	leaf, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		return facts, err
	}
	clientKey, err := bundle.trust.VerifyMember(leaf, time.Now().UTC())
	if err != nil || clientKey != info.KeyFingerprint {
		return facts, errors.Join(err, errors.New("sparse discovery client certificate differs from persisted member"))
	}
	// DialContext verifies the actual destination SAN and policy. The observed
	// key is taken from this TLS connection, not from a seed cache or hint.
	observeDial := func(ctx context.Context, address string) (net.Conn, string, error) {
		conn, err := (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", address)
		if err != nil || conn == nil {
			return conn, "", err
		}
		state := conn.(*tls.Conn).ConnectionState()
		fingerprint, err := bundle.trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		return conn, fingerprint, err
	}
	expected := store.ClusterIdentity{ClusterID: info.ClusterID, CAFingerprint: info.CAFingerprint, Protocol: info.Protocol}
	contacts, err := discoverSparseRaftContacts(ctx, seeds, func(ctx context.Context, seed string) (map[string]string, error) {
		provider, providerKey, err := cluster.ObserveRaftLeader(ctx, seed, observeDial)
		if err != nil {
			return nil, err
		}
		pinned := func(ctx context.Context, address string) (net.Conn, error) {
			if address != provider.Address {
				return nil, errors.New("sparse directory refuses changed provider endpoint")
			}
			conn, actualKey, err := observeDial(ctx, address)
			if actualKey != providerKey {
				err = errors.Join(err, errors.New("sparse directory actual TLS key changed"))
			}
			return conn, err
		}
		_, members, err := store.ReadRemoteClusterDiscovery(ctx, expected, provider, providerKey, info.KeyFingerprint, filepath.Base(path), pinned)
		if err != nil {
			return nil, err
		}
		return sparseDiscoveryContacts(members, policy.AllowedKeys)
	})
	if err != nil {
		return facts, err
	}
	// Raft workers and original sockets have actually joined before HTTP work.
	caller, err := enrollment.NewClient(info.EnrollmentURL, info.ClusterID, info.CAFingerprint, info.Protocol)
	if err != nil {
		return facts, err
	}
	defer caller.Close()
	return caller.DiscoverClusterQuorumFactsFromContacts(ctx, certificate, key, policy.AllowedKeys, contacts)
}
