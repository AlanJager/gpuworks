//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func startWaitingSeedRaft(ctx context.Context, cfg Config, bundle *enrollmentBundle) (joinedRaftSession, error) {
	if bundle == nil || bundle.trust == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.tls.Certificates[0].Leaf == nil {
		return nil, errors.New("expanded seed requires a verified local member certificate")
	}
	key, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	return store.StartEstablishedRaft(ctx, cfg.DBPath, expected, key)
}
