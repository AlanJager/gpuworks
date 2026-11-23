//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// clusterMigrationRun is the one schema migration a member runs at a time.
// It outlives the request that started it: a migration can take longer than
// a caller waits, and cancelling it part way would only repeat the work.
type clusterMigrationRun struct {
	mu   sync.Mutex
	done chan struct{}
	err  error
}

const (
	clusterMigrationBudget = 30 * time.Minute
	clusterMigrationWait   = 6 * time.Second
	// clusterLeadWait bounds one attempt to take the database leadership.
	// It is shorter than a caller waits for the step, so the caller gets
	// the reason.
	clusterLeadWait = 10 * time.Second
)

// SetClusterUpgradeExtensions tells the session which extension migration
// sets this executable includes. The upgrade steps report and apply them with
// the core list. Call it before the member classifies its schema.
func (s *LegacyJoinedRaftSession) SetClusterUpgradeExtensions(sets []MigrationSet) error {
	if err := validateMigrationSets(sets); err != nil {
		return err
	}
	s.upgradeMu.Lock()
	s.upgradeSets = append([]MigrationSet(nil), sets...)
	s.upgradeMu.Unlock()
	return nil
}

func (s *LegacyJoinedRaftSession) clusterUpgradeSteps(st *Store) ClusterUpgradeSteps {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	return st.ClusterUpgradeFor(s.upgradeSets)
}

// ReportClusterUpgradeCapability records this member's executable for the
// cluster upgrade, through the session that still owns the database.
func (s *LegacyJoinedRaftSession) ReportClusterUpgradeCapability(ctx context.Context) error {
	st, key, _, err := s.clusterUpgradeStore(ctx)
	if err != nil {
		return err
	}
	return s.clusterUpgradeSteps(st).Report(ctx, key)
}

// ClusterUpgrade performs one upgrade step for an admitted member. Status is
// answered by any member, and lead makes the answering member the database
// leader; the other steps act on the database leader only and are refused
// elsewhere, with the state they found. migrating is true while a
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
	steps := s.clusterUpgradeSteps(st)
	if action == "status" {
		s.migration.mu.Lock()
		migrating = s.migration.done != nil
		s.migration.mu.Unlock()
	}
	if action == "lead" {
		status, leader, err = leadClusterUpgradeStep(ctx, steps.Status, leader, func(ctx context.Context) error {
			return TakeDatabaseLeadership(ctx, s.live.Load())
		})
		return status, leader, false, err
	}
	if action != "status" && !leader {
		status, err = steps.Status(ctx)
		if err == nil {
			err = refuseClusterUpgrade("this member is not the database leader")
		}
		return status, leader, false, err
	}
	switch action {
	case "status":
		status, err = steps.Status(ctx)
	case "begin":
		status, err = steps.Begin(ctx)
	case "finish":
		status, err = steps.Finish(ctx)
	case "abort":
		status, err = steps.Abort(ctx)
	case "migrate":
		migrating, err = s.runClusterMigration(ctx, steps)
		if observed, statusErr := steps.Status(ctx); statusErr == nil {
			status = observed
		} else if err == nil && !migrating {
			err = statusErr
		}
	default:
		err = errors.New("unknown cluster upgrade step")
	}
	return status, leader, migrating, err
}

// runClusterMigration starts the migration unless one is running, and waits a
// short while for it. It reports true while the run continues.
func (s *LegacyJoinedRaftSession) runClusterMigration(ctx context.Context, steps ClusterUpgradeSteps) (bool, error) {
	run := &s.migration
	run.mu.Lock()
	if run.done == nil {
		done := make(chan struct{})
		run.done, run.err = done, nil
		go func() {
			budget, cancel := context.WithTimeout(context.Background(), clusterMigrationBudget)
			_, err := steps.Migrate(budget)
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

// TakeDatabaseLeadership asks the database leader to hand over to the member
// behind db and returns once that member leads. The member that led until
// then can afterwards be stopped without the others waiting out an election.
func TakeDatabaseLeadership(ctx context.Context, db *cluster.Database) error {
	if db == nil || db.ID == 0 {
		return errors.New("native session is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, clusterLeadWait)
	defer cancel()
	return clusterLeadership{
		Self: db.ID,
		Leader: func(ctx context.Context) (uint64, error) {
			member, err := db.Leader(ctx)
			return member.ID, err
		},
		Transfer: db.Transfer,
	}.take(ctx)
}

// clusterUpgradeStore binds the session's pools for one upgrade step. The
// session keeps the pools; the returned Store must not be closed.
func (s *LegacyJoinedRaftSession) clusterUpgradeStore(ctx context.Context) (*Store, string, bool, error) {
	if s == nil {
		return nil, "", false, errors.New("native session is unavailable")
	}
	db := s.live.Load()
	if db == nil || db.ID == 0 || db.SQL == nil || db.Scheduling == nil {
		return nil, "", false, errors.New("native session is unavailable")
	}
	st, err := OpenReplicated(ctx, db.SQL, db.Scheduling, nil, ReplicatedBackend())
	if err != nil {
		return nil, "", false, err
	}
	current, err := db.Leader(ctx)
	if err != nil {
		return nil, "", false, err
	}
	return st, db.TransportMemberKey(), current.ID == db.ID, nil
}

// ClusterUpgradeProgress observes the upgrade facts for a reader that proves
// nothing: it changes nothing and reports whether a migration this member
// started is running. The caller decides what of it to show.
func (s *LegacyJoinedRaftSession) ClusterUpgradeProgress(ctx context.Context) (status ClusterUpgradeStatus, migrating bool, err error) {
	st, _, _, err := s.clusterUpgradeStore(ctx)
	if err != nil {
		return status, false, err
	}
	s.migration.mu.Lock()
	migrating = s.migration.done != nil
	s.migration.mu.Unlock()
	status, err = s.clusterUpgradeSteps(st).Status(ctx)
	return status, migrating, err
}
