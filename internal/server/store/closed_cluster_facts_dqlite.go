//go:build linux && dqlite

package store

import (
	"context"
	"database/sql"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReadLocalQuorumFacts observes the original live native session, including
// after Store takes responsibility for its eventual Close. Ownership transfer
// does not replace that Database or authorize serving from these facts.
// requesterKey must come from a verified live transport; this method does not
// authenticate a certificate or authorize later serving/migration/handoff.
func (s *LegacyJoinedRaftSession) ReadLocalQuorumFacts(ctx context.Context, requesterKey string) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
	fail := func(err error) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
		return false, MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if s == nil {
		return fail(errors.New("native session is unavailable"))
	}
	if !s.mu.TryLock() {
		return fail(errors.Join(ctx.Err(), errors.New("native session is classifying or closing")))
	}
	db, id, address, raftDir := s.db, s.id, s.address, s.raftDir
	s.mu.Unlock()
	if db == nil || db.ID != id || db.Address != address || id == 0 {
		return fail(errors.New("native session is unavailable"))
	}
	clusterID, pin := db.TransportIdentity()
	expected := ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: ClusterProtocolVersion}
	providerKey := db.TransportMemberKey()
	var legacy bool
	var phase MaintenancePhaseState
	var admissions []ClusterAdmission
	proof, err := db.WithLocalQuorumReadSnapshot(ctx, func(ctx context.Context, tx *sql.Tx, first cluster.QuorumProof) error {
		gormTx, err := bindClosedFactsTx(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := classifyEstablishedRuntimeSnapshot(ctx, gormTx, expected, providerKey, address, raftDir); err != nil {
			return err
		}
		legacy, phase, admissions, err = readClosedClusterFactsDB(ctx, gormTx, expected, providerKey, requesterKey, first)
		return err
	})
	if err != nil {
		return fail(err)
	}
	if !s.mu.TryLock() {
		return fail(errors.Join(ctx.Err(), errors.New("native session is classifying or closing")))
	}
	available := s.db == db
	s.mu.Unlock()
	if !available {
		return fail(errors.New("native session retired during facts read"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return legacy, phase, admissions, proof, nil
}
