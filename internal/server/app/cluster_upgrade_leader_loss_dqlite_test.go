//go:build linux && dqlite

package app

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// upgradeLeader returns the member the others follow, and a Store on it.
func upgradeLeader(t *testing.T, ctx context.Context, databases []*cluster.Database) (int, *store.Store) {
	t.Helper()
	index := -1
	removalWait(t, ctx, "a database leader", func() bool {
		for i, db := range databases {
			if db == nil {
				continue
			}
			if leader, err := db.Leader(ctx); err == nil && leader.ID == db.ID {
				index = i
				return true
			}
		}
		return false
	})
	st, err := store.OpenReplicated(ctx, databases[index].SQL, databases[index].Scheduling, bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatal(err)
	}
	return index, st
}

// The leader is lost while it migrates, after one step committed and inside
// the next. The step is repeated on the new leader: it applies only what is
// left, and the cluster returns to serving with two of three members.
func TestClusterUpgradeResumesOnANewLeaderAfterAPartialMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	databases := removalDatabases(t, ctx)
	leader, st := upgradeLeader(t, ctx, databases)
	if err := st.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keys := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i, key := range keys {
		invitation := ""
		if i > 0 {
			invitation = strings.Repeat(fmt.Sprint(i), 64)
		}
		if _, err := databases[leader].Scheduling.ExecContext(ctx, `INSERT INTO cluster_admissions (key_fingerprint, invitation_hash, address, enrollment_origin, admitted_at, material_grant_sequence)
			VALUES (?, ?, ?, '', CURRENT_TIMESTAMP, 0)`, key, invitation, databases[i].Address); err != nil {
			t.Fatal(err)
		}
	}

	var first, second atomic.Int32
	inSecond := make(chan struct{})
	hold := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	sets := []store.MigrationSet{{Namespace: "probe", TablePrefix: "ee_probe_", Migrations: []store.Migration{
		{Version: 1, Name: "first", Apply: func(tx *gorm.DB) error {
			first.Add(1)
			return tx.Exec(`CREATE TABLE ee_probe_first (id INTEGER PRIMARY KEY)`).Error
		}},
		{Version: 2, Name: "second", Apply: func(tx *gorm.DB) error {
			if err := tx.Exec(`CREATE TABLE ee_probe_second (id INTEGER PRIMARY KEY)`).Error; err != nil {
				return err
			}
			if second.Add(1) == 1 {
				close(inSecond)
				<-hold
			}
			return nil
		}},
	}}}
	steps := st.ClusterUpgradeFor(sets)
	for _, key := range keys {
		if err := steps.Report(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := steps.Begin(ctx); err != nil || status.Phase.Phase != store.MaintenanceUpgrading {
		t.Fatalf("begin = %+v, %v", status.Phase, err)
	}
	migrated := make(chan error, 1)
	go func() {
		_, err := steps.Migrate(ctx)
		migrated <- err
	}()
	select {
	case <-inSecond:
	case err := <-migrated:
		t.Fatalf("migrate ended before its second step: %v", err)
	case <-ctx.Done():
		t.Fatal("the second step never started")
	}
	// The leader's database process goes away inside the second step.
	lost := databases[leader]
	databases[leader] = nil
	if err := lost.Close(); err != nil {
		t.Fatal(err)
	}
	close(hold)
	if err := <-migrated; err == nil {
		t.Fatal("migrate on the lost leader reported success")
	}

	next, resumed := upgradeLeader(t, ctx, databases)
	if next == leader {
		t.Fatal("the lost member is still the leader")
	}
	steps = resumed.ClusterUpgradeFor(sets)
	var status store.ClusterUpgradeStatus
	removalWait(t, ctx, "the upgrade status on the new leader", func() bool {
		var err error
		status, err = steps.Status(ctx)
		return err == nil
	})
	if status.Phase.Phase != store.MaintenanceUpgrading || status.AppliedExtensions != 1 || status.Pending() != 1 || !status.Ready() {
		t.Fatalf("new leader sees %+v; want upgrading with the first step applied and one pending", status)
	}
	if _, err := steps.Abort(ctx); err == nil {
		t.Fatal("abort after a committed step succeeded")
	}
	status, err := steps.Migrate(ctx)
	if err != nil || status.Pending() != 0 {
		t.Fatalf("repeated migrate = pending %d, %v", status.Pending(), err)
	}
	if first.Load() != 1 || second.Load() != 2 {
		t.Fatalf("steps ran first=%d second=%d; want the committed step once and the interrupted one twice", first.Load(), second.Load())
	}
	if status, err = steps.Finish(ctx); err != nil || status.Phase.Phase != store.MaintenanceServing {
		t.Fatalf("finish = %+v, %v", status.Phase, err)
	}
	var tables int
	if err := databases[next].SQL.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name IN ('ee_probe_first', 'ee_probe_second')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("probe tables = %d, %v; want both", tables, err)
	}
}
