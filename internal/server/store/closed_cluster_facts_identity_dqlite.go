//go:build linux && dqlite

package store

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ReadLocalQuorumFactsForCluster binds facts to the authenticated listener's
// cluster identity. The original session cannot substitute a different native
// Database between this check and the completed observation.
func (s *LegacyJoinedRaftSession) ReadLocalQuorumFactsForCluster(ctx context.Context, expected ClusterIdentity, requesterKey string) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
	fail := func(err error) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
		return false, MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if s == nil {
		return fail(ErrClusterIdentityConflict)
	}
	if !s.mu.TryLock() {
		return fail(errors.Join(ctx.Err(), errors.New("native session is classifying or closing")))
	}
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fail(errors.New("native session is unavailable"))
	}
	if err := observedClusterIdentity(db, expected); err != nil {
		return fail(err)
	}
	legacy, phase, admissions, proof, err := s.ReadLocalQuorumFacts(ctx, requesterKey)
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
