package store

import (
	"context"
	"database/sql"
	"errors"
	"net"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReadRemoteClusterMaterials observes bootstrap prerequisites through an
// existing read-only remote database. Both native barriers and all SQL/socket
// cleanup finish before returning; the result never grants business authority.
func ReadRemoteClusterMaterials(ctx context.Context, expected ClusterIdentity, provider cluster.Member, providerKey, requesterKey, databaseName string, dial func(context.Context, string) (net.Conn, error)) (MaintenancePhaseState, []ClusterTransportMember, []MaterialManifestRecord, error) {
	if err := ctx.Err(); err != nil {
		return MaintenancePhaseState{}, nil, nil, err
	}
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return MaintenancePhaseState{}, nil, nil, ErrClusterIdentityConflict
	}
	if !validClusterDigest(providerKey, 32) || !validClusterDigest(requesterKey, 32) {
		return MaintenancePhaseState{}, nil, nil, ErrClusterAdmissionRejected
	}
	var phase MaintenancePhaseState
	var members []ClusterTransportMember
	var pointers []MaterialManifestRecord
	_, err := cluster.ReadRemoteExistingReadSnapshot(ctx, provider, databaseName, dial, func(ctx context.Context, tx *sql.Tx, first cluster.QuorumProof) error {
		var err error
		phase, members, pointers, err = readClosedClusterMaterials(ctx, tx, expected, provider, providerKey, requesterKey, first.Members)
		return err
	})
	if err != nil {
		return MaintenancePhaseState{}, nil, nil, err
	}
	return phase, members, pointers, nil
}

func readClosedClusterMaterials(ctx context.Context, tx *sql.Tx, expected ClusterIdentity, provider cluster.Member, providerKey, requesterKey string, native []cluster.Member) (MaintenancePhaseState, []ClusterTransportMember, []MaterialManifestRecord, error) {
	fail := func(err error) (MaintenancePhaseState, []ClusterTransportMember, []MaterialManifestRecord, error) {
		return MaintenancePhaseState{}, nil, nil, err
	}
	_, members, err := readClosedClusterDiscovery(ctx, tx, expected, provider, providerKey, requesterKey, native)
	if err != nil {
		return fail(err)
	}
	if len(members) != 3 {
		return fail(errors.New("bootstrap materials require exactly three live voters"))
	}
	requesterLive := false
	ids := make([]int64, 0, len(members))
	for _, member := range members {
		if member.Role != "voter" || member.EnrollmentOrigin == "" {
			return fail(errors.New("bootstrap material member lacks a voter role or HTTPS origin"))
		}
		requesterLive = requesterLive || member.KeyFingerprint == requesterKey
		ids = append(ids, int64(member.ID))
	}
	if !requesterLive {
		return fail(ErrClusterAdmissionRejected)
	}
	db, err := bindClosedFactsTx(ctx, tx)
	if err != nil {
		return fail(err)
	}
	phase, err := (&Store{db: db}).ReadMaintenancePhase(ctx)
	if err != nil {
		return fail(err)
	}
	if err := requireServingClusterPhase(phase); err != nil {
		return fail(err)
	}
	var pointers []MaterialManifestRecord
	if err := db.Where("source_member_id IN ?", ids).Order("source_member_id").Find(&pointers).Error; err != nil {
		return fail(err)
	}
	if len(pointers) != len(members) {
		return fail(errors.New("bootstrap material source manifests are incomplete"))
	}
	for _, member := range members {
		found := false
		for _, pointer := range pointers {
			if uint64(pointer.SourceMemberID) != member.ID {
				continue
			}
			if found || pointer.ClusterID != expected.ClusterID || pointer.SourceKey != member.KeyFingerprint || !materialDigest(pointer.Digest) || pointer.Bytes <= 0 || pointer.Bytes > 16<<20 {
				return fail(errors.New("bootstrap material pointer differs from its current member"))
			}
			found = true
		}
		if !found {
			return fail(errors.New("bootstrap material source manifest is missing"))
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return phase, members, pointers, nil
}
