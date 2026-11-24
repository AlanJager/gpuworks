//go:build linux && dqlite

package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A member that asks takes the database leadership from the member that
// leads, on the real database: every member then names it as the leader, a
// write through the member that led before commits, asking again changes
// nothing, and once the member that led before has stopped the two others
// still commit.
func TestDatabaseLeadershipIsHandedToTheMemberThatAsks(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	databases := removalDatabases(t, ctx)
	before := -1
	removalWait(t, ctx, "a database leader", func() bool {
		for i, db := range databases {
			if leader, err := db.Leader(ctx); err == nil && leader.ID == db.ID {
				before = i
				return true
			}
		}
		return false
	})
	next := (before + 1) % len(databases)

	started := time.Now()
	// On a loaded machine the leader can change by itself while the test
	// runs; the handover is asked again until this member leads.
	removalWait(t, ctx, "the handover", func() bool {
		return store.TakeDatabaseLeadership(ctx, databases[next]) == nil
	})
	t.Logf("handover took %s", time.Since(started).Round(time.Millisecond))
	removalWait(t, ctx, "every member naming the new leader", func() bool {
		for _, db := range databases {
			leader, err := db.Leader(ctx)
			if err != nil || leader.ID != databases[next].ID {
				return false
			}
		}
		return true
	})
	// The member that led before now follows, and a write through it commits.
	removalWait(t, ctx, "a write through the member that led before", func() bool {
		_, err := databases[before].SQL.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS handover_probe (id INTEGER PRIMARY KEY)`)
		return err == nil
	})

	// Asking again leaves the leader where it is.
	if err := store.TakeDatabaseLeadership(ctx, databases[next]); err != nil {
		t.Fatalf("a member that leads asked again: %v", err)
	}
	if leader, err := databases[next].Leader(ctx); err != nil || leader.ID != databases[next].ID {
		t.Fatalf("after asking again the member names leader %d, %v; want itself", leader.ID, err)
	}

	// The member that led before stops; the two others still commit.
	stopped := databases[before]
	databases[before] = nil
	if err := stopped.Close(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	removalWait(t, ctx, "a write after the former leader stopped", func() bool {
		_, err := databases[next].SQL.ExecContext(ctx, `INSERT OR IGNORE INTO handover_probe (id) VALUES (1)`)
		return err == nil
	})
	kept := false
	if leader, err := databases[next].Leader(ctx); err == nil {
		kept = leader.ID == databases[next].ID
	}
	t.Logf("first write after the former leader stopped took %s; the leader stayed: %v", time.Since(started).Round(time.Millisecond), kept)
}
