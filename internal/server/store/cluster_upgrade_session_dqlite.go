//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// clusterMigrationRun is the one schema migration a member runs at a time.
// It outlives the request that started it: a migration can take longer than
// a caller waits, and cancelling it part way would only repeat the work.
type clusterMigrationRun struct {
	mu   sync.Mutex
	done chan struct{}
	err  error
}

var clusterMigrationRuns sync.Map // *LegacyJoinedRaftSession -> *clusterMigrationRun

const (
	clusterMigrationBudget = 30 * time.Minute
	clusterMigrationWait   = 6 * time.Second
)

// ReportClusterUpgradeCapability records this member's executable for the
// cluster upgrade, through the session that still owns the database.
func (s *LegacyJoinedRaftSession) ReportClusterUpgradeCapability(ctx context.Context) error {
	st, key, _, err := s.clusterUpgradeStore(ctx)
	if err != nil {
		return err
	}
	return st.ReportClusterUpgradeCapability(ctx, key)
}

// ClusterUpgrade performs one upgrade step for an admitted member. Status is
// answered by any member; the other steps act on the database leader only and
// are refused elsewhere, with the state they found. migrating is true while a
// migration this member started is still running; the caller asks again.
func (s *LegacyJoinedRaftSession) ClusterUpgrade(ctx context.Context, expected ClusterIdentity, requesterKey, action string) (status ClusterUpgradeStatus, leader, migrating bool, err error) {
	st, _, leader, err := s.clusterUpgradeStore(ctx)
	if err != nil {
		return status, false, false, err
	}
	actual, err := st.ClusterIdentity(ctx)
	if err != nil {
		return status, leader, false, err
	}
	if actual.ClusterID != expected.ClusterID || actual.CAFingerprint != expected.CAFingerprint || actual.Protocol != expected.Protocol {
		return status, leader, false, ErrClusterIdentityConflict
	}
	var admitted int
	if err := st.db.WithContext(ctx).Raw(`SELECT count(*) FROM cluster_admissions WHERE key_fingerprint = ? AND revoked_at IS NULL`, requesterKey).Row().Scan(&admitted); err != nil {
		return status, leader, false, err
	}
	if admitted != 1 {
		return status, leader, false, ErrClusterAdmissionRejected
	}
	if action != "status" && !leader {
		status, err = st.ClusterUpgradeStatus(ctx)
		if err == nil {
			err = refuseClusterUpgrade("this member is not the database leader")
		}
		return status, leader, false, err
	}
	switch action {
	case "status":
		status, err = st.ClusterUpgradeStatus(ctx)
	case "begin":
		status, err = st.BeginClusterUpgrade(ctx)
	case "finish":
		status, err = st.FinishClusterUpgrade(ctx)
	case "abort":
		status, err = st.AbortClusterUpgrade(ctx)
	case "migrate":
		migrating, err = s.runClusterMigration(ctx, st)
		if err == nil {
			status, err = st.ClusterUpgradeStatus(ctx)
		} else if observed, statusErr := st.ClusterUpgradeStatus(ctx); statusErr == nil {
			status = observed
		}
	default:
		err = errors.New("unknown cluster upgrade step")
	}
	return status, leader, migrating, err
}

// runClusterMigration starts the migration unless one is running, and waits a
// short while for it. It reports true while the run continues.
func (s *LegacyJoinedRaftSession) runClusterMigration(ctx context.Context, st *Store) (bool, error) {
	value, _ := clusterMigrationRuns.LoadOrStore(s, &clusterMigrationRun{})
	run := value.(*clusterMigrationRun)
	run.mu.Lock()
	if run.done == nil {
		done := make(chan struct{})
		run.done, run.err = done, nil
		go func() {
			budget, cancel := context.WithTimeout(context.Background(), clusterMigrationBudget)
			_, err := st.MigrateClusterSchema(budget)
			cancel()
			run.mu.Lock()
			run.err = err
			run.mu.Unlock()
			close(done)
		}()
	}
	done := run.done
	run.mu.Unlock()
	timer := time.NewTimer(clusterMigrationWait)
	defer timer.Stop()
	select {
	case <-done:
		run.mu.Lock()
		err := run.err
		if run.done == done {
			run.done = nil
		}
		run.mu.Unlock()
		return false, err
	case <-timer.C:
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

// clusterUpgradeStore binds the session's pools for one upgrade step. The
// session keeps the pools; the returned Store must not be closed.
func (s *LegacyJoinedRaftSession) clusterUpgradeStore(ctx context.Context) (*Store, string, bool, error) {
	if s == nil {
		return nil, "", false, errors.New("native session is unavailable")
	}
	// A member that waits for the upgrade classifies the schema again every
	// second and holds the session meanwhile; wait for a gap between attempts.
	for deadline := time.Now().Add(3 * time.Second); !s.mu.TryLock(); {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, "", false, errors.Join(ctx.Err(), errors.New("native session is classifying or closing"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	db := s.db
	s.mu.Unlock()
	if db == nil || db.ID == 0 || db.SQL == nil || db.Scheduling == nil {
		return nil, "", false, errors.New("native session is unavailable")
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, nil)
	if err != nil {
		return nil, "", false, err
	}
	current, err := db.Leader(ctx)
	if err != nil {
		return nil, "", false, err
	}
	return st, db.TransportMemberKey(), current.ID == db.ID, nil
}
