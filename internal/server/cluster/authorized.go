package cluster

import (
	"context"
	"crypto/x509"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// StartAuthorizedDatabase loads a durable local authorization projection before
// starting Raft. It never queries that Raft database to authorize a handshake.
// The owner must hold its runtime lease and install the policy only through an
// authorized initialization/join operation. Only additive source snapshots can
// be applied while running; revocation requires a separate operation that also
// terminates established connections.
func StartAuthorizedDatabase(ctx context.Context, cfg DatabaseConfig, policyPath string, trust *identity.Trust, certificate, key []byte) (result *Database, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if trust == nil || cfg.ListenTLS != nil || cfg.DialTLS != nil || cfg.TransportPolicyPath != "" {
		return nil, errors.New("authorized database requires pinned trust and policy-derived TLS only")
	}
	owner, err := identity.OpenRuntimeTransportPolicy(policyPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			resultErr = errors.Join(resultErr, owner.Close())
		}
	}()
	cfg.ListenTLS, cfg.DialTLS, err = owner.TLSConfigs(trust, certificate, key, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	cfg.TLSCertPEM, cfg.TLSKeyPEM, cfg.TLSCACertPEM = certificate, key, trust.CACertificatePEM()
	cfg.TransportPolicyPath = policyPath
	local, err := x509.ParseCertificate(cfg.ListenTLS.Certificates[0].Certificate[0])
	if err != nil {
		return nil, err
	}
	fingerprint, err := trust.VerifyMember(local, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	policy, err := owner.Snapshot()
	if err != nil {
		return nil, err
	}
	db, err := StartDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	db.transportClusterID = policy.ClusterID
	db.transportCAFingerprint = policy.CAFingerprint
	db.transportKeyFingerprint = fingerprint
	db.transportPolicy = owner
	stop := db.stop
	db.stop = func() error { return errors.Join(stop(), owner.Close()) }
	return db, nil
}

// OpenAuthorizedDatabase waits for ordinary SQL access and releases transport
// and policy ownership if readiness fails.
func OpenAuthorizedDatabase(ctx context.Context, cfg DatabaseConfig, policyPath string, trust *identity.Trust, certificate, key []byte) (*Database, error) {
	db, err := StartAuthorizedDatabase(ctx, cfg, policyPath, trust, certificate, key)
	if err != nil {
		return nil, err
	}
	if err := db.WaitReady(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}
