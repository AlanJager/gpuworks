package store

import (
	"context"
	"database/sql"
	"net"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReadRemoteClusterDiscovery reads public directory hints without opening a
// local native database or a Store. dial must authenticate and pin providerKey
// on every physical connection, including proofs A and B. A caller borrowing a
// local runtime lease must retain it until this function's cleanup has returned.
// These hints confer neither startup authority nor an admitted live spare role.
func ReadRemoteClusterDiscovery(ctx context.Context, expected ClusterIdentity, provider cluster.Member, providerKey, requesterKey, databaseName string, dial func(context.Context, string) (net.Conn, error)) (ClusterAdmission, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return ClusterAdmission{}, nil, err
	}
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return ClusterAdmission{}, nil, ErrClusterIdentityConflict
	}
	if !validClusterDigest(providerKey, 32) || !validClusterDigest(requesterKey, 32) {
		return ClusterAdmission{}, nil, ErrClusterAdmissionRejected
	}
	var requester ClusterAdmission
	var members []ClusterTransportMember
	_, err := cluster.ReadRemoteExistingReadSnapshot(ctx, provider, databaseName, dial, func(ctx context.Context, tx *sql.Tx, first cluster.QuorumProof) error {
		var err error
		requester, members, err = readClosedClusterDiscovery(ctx, tx, expected, provider, providerKey, requesterKey, first.Members)
		return err
	})
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	return requester, members, nil
}
