package store

import (
	"context"
	"database/sql"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// This fixed reader returns dial hints from a borrowed transaction. Neither
// the native discovery input nor the admission snapshot proves fresh quorum.
func readClosedClusterDiscovery(ctx context.Context, tx *sql.Tx, expected ClusterIdentity, provider cluster.Member, providerKey, requesterKey string, native []cluster.Member) (ClusterAdmission, []ClusterTransportMember, error) {
	db, err := bindClosedFactsTx(ctx, tx)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	if !validClusterDigest(providerKey, 32) || !validClusterDigest(requesterKey, 32) || provider.ID == 0 {
		return ClusterAdmission{}, nil, ErrClusterAdmissionRejected
	}
	if _, err := classifyJoinedSchemaClass(ctx, db, expected); err != nil {
		return ClusterAdmission{}, nil, err
	}
	admissions, err := clusterTransportAdmissions(db)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	native, err = normalizeLiveClusterMembers(native)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	members, err := bindLiveClusterAdmissions(native, admissions)
	if err != nil {
		return ClusterAdmission{}, nil, err
	}
	providerFound := false
	for _, member := range members {
		if member.ID == provider.ID && member.Address == provider.Address && member.KeyFingerprint == providerKey {
			providerFound = true
		}
	}
	var requester ClusterAdmission
	requesterFound := false
	for _, admission := range admissions {
		if admission.KeyFingerprint == requesterKey && admission.RevokedAt == nil {
			requester, requesterFound = admission, true
		}
	}
	if !providerFound || !requesterFound {
		return ClusterAdmission{}, nil, ErrClusterAdmissionRejected
	}
	if err := ctx.Err(); err != nil {
		return ClusterAdmission{}, nil, err
	}
	return requester, members, nil
}
