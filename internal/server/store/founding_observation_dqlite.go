//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// The member that created a cluster keeps serving in the process that
// opened the database as a single member, for as long as that process runs:
// it becomes an established member, with a session, only at its next start.
// Other members and the cluster's commands ask every member the same
// observation questions, so this process answers them too, from its open
// store. foundingDatabases holds the database handle of such a store.
var foundingDatabases sync.Map

type foundingDatabase struct {
	db      *cluster.Database
	raftDir string
}

// ErrClusterUpgradeFoundingProcess refuses an upgrade step that changes the
// cluster on the process that created it. Such a step runs on an established
// member's session, which owns the migration; restarting this member once
// makes it one. Callers test for it with errors.Is; it is also a refusal.
var ErrClusterUpgradeFoundingProcess = refuseClusterUpgrade("this member still runs the process that created the cluster; restart it once before it takes part in an upgrade")

// FoundingObservation answers, on the process that created the cluster, the
// observation questions an established member's session answers: the quorum
// facts, the discovery of the members, and the upgrade status. It reads
// through the same functions as the session and changes nothing.
type FoundingObservation struct {
	st      *Store
	db      *cluster.Database
	raftDir string
	sets    []MigrationSet
}

// FoundingObservation is the store's observation when the store opened a
// database with a cluster identity itself. sets are the executable's
// extension migration sets, for the upgrade status.
func (s *Store) FoundingObservation(sets []MigrationSet) (*FoundingObservation, bool) {
	value, ok := foundingDatabases.Load(s)
	if !ok {
		return nil, false
	}
	founding := value.(foundingDatabase)
	return &FoundingObservation{st: s, db: founding.db, raftDir: founding.raftDir, sets: append([]MigrationSet(nil), sets...)}, true
}

// Close releases nothing: the store owns the database.
func (o *FoundingObservation) Close() error { return nil }

// FoundingProcess marks the answers of the process that created the cluster.
func (o *FoundingObservation) FoundingProcess() bool { return true }

// ReadLocalQuorumFactsForCluster observes the cluster for a member that
// names this database's cluster.
func (o *FoundingObservation) ReadLocalQuorumFactsForCluster(ctx context.Context, expected ClusterIdentity, requesterKey string) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
	fail := func(err error) (bool, MaintenancePhaseState, []ClusterAdmission, cluster.QuorumProof, error) {
		return false, MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	clusterID, pin := o.db.TransportIdentity()
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion ||
		expected.ClusterID != clusterID || expected.CAFingerprint != pin {
		return fail(ErrClusterIdentityConflict)
	}
	legacy, phase, admissions, proof, err := readLocalQuorumFacts(ctx, o.db, o.db.Address, o.raftDir, requesterKey)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return legacy, phase, admissions, proof, nil
}

// ReadQuorumDiscoveryForCluster reads the members' public dial hints.
func (o *FoundingObservation) ReadQuorumDiscoveryForCluster(ctx context.Context, expected ClusterIdentity, requesterKey string) (ClusterAdmission, []ClusterTransportMember, error) {
	if err := ctx.Err(); err != nil {
		return ClusterAdmission{}, nil, err
	}
	if !validClusterDigest(expected.ClusterID, 16) || !validClusterDigest(expected.CAFingerprint, 32) || expected.Protocol != ClusterProtocolVersion {
		return ClusterAdmission{}, nil, ErrClusterIdentityConflict
	}
	return readQuorumDiscovery(ctx, o.db, expected, requesterKey)
}

// ClusterUpgrade answers the upgrade status for an admitted member and
// refuses every step that changes the cluster with
// ErrClusterUpgradeFoundingProcess, returning the status with the refusal.
func (o *FoundingObservation) ClusterUpgrade(ctx context.Context, expected ClusterIdentity, requesterKey, action string) (status ClusterUpgradeStatus, leader, migrating bool, err error) {
	current, err := o.db.Leader(ctx)
	if err != nil {
		return status, false, false, err
	}
	leader = current.ID == o.db.ID
	if err := admitClusterUpgradeCaller(ctx, o.st, expected, requesterKey); err != nil {
		return status, leader, false, err
	}
	status, err = o.st.ClusterUpgradeFor(o.sets).Status(ctx)
	if err == nil && action != "status" {
		err = ErrClusterUpgradeFoundingProcess
	}
	return status, leader, false, err
}

// bindFoundingDatabase records the handle of a database this store opened
// with a cluster identity, until the store closes it.
func bindFoundingDatabase(st *Store, db *cluster.Database, raftDir string) {
	foundingDatabases.Store(st, foundingDatabase{db: db, raftDir: raftDir})
	closeDatabase := st.closeBackend
	st.closeBackend = func() error {
		foundingDatabases.Delete(st)
		if closeDatabase == nil {
			return errors.New("founding database has no owner to close it")
		}
		return closeDatabase()
	}
}
