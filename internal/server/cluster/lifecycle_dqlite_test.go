//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func TestDatabaseStartedLifecycleSurvivesReadinessFailure(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	// Three members: the database process keeps a single voter in any smaller
	// cluster, so a lost quorum and its return need three voters.
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 3)
	for _, f := range fixtures {
		if err := identity.PublishTransportPolicy(f.policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
	}
	first, second, third := fixtures[0], fixtures[1], fixtures[2]
	second.config.Seeds = []string{first.config.Address}
	third.config.Seeds = []string{first.config.Address}
	if err := os.Chmod(filepath.Dir(second.config.Dir), 0700); err != nil {
		t.Fatal(err)
	}
	preparedID, err := PrepareJoinIdentity(ctx, second.config.Dir, second.config.Address, second.config.Seeds)
	if err != nil {
		t.Fatal(err)
	}

	b, err := StartAuthorizedDatabase(ctx, second.config, second.policyPath, trust, second.certificate, second.key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	short, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	if err := b.WaitReady(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing seed: %v", err)
	}
	stop()
	a := openAuthorizedFixture(t, ctx, trust, first)
	if err := b.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	id := b.ID
	if id != preparedID {
		t.Fatal("prepared identity not used")
	}
	c := openAuthorizedFixture(t, ctx, trust, third)
	conversionWait(t, ctx, "three voters", func() bool {
		members, err := a.Members(ctx)
		return err == nil && len(members) == 3 && members[0].Role == "voter" && members[1].Role == "voter" && members[2].Role == "voter"
	})
	// Two of three voters stop: the one left has no quorum.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	short, stop = context.WithTimeout(ctx, 150*time.Millisecond)
	if _, err := a.SQL.ExecContext(short, "CREATE TABLE no_quorum_fixture (id INTEGER PRIMARY KEY)"); err == nil {
		t.Fatal("write succeeded without quorum")
	}
	stop()
	startCtx, stopStart := context.WithCancel(ctx)
	b, err = StartAuthorizedDatabase(startCtx, second.config, second.policyPath, trust, second.certificate, second.key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	stopStart() // Start's context must not own the returned process.
	if b.ID != id {
		t.Fatalf("changed identity %d -> %d", id, b.ID)
	}
	if err := b.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("restored member ready")
	lifecycleWrite(t, ctx, b.SQL, "recovered write", "CREATE TABLE IF NOT EXISTS recovered_fixture (id INTEGER PRIMARY KEY)")
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleWrite(t, ctx, a.SQL, "write on the member that stayed", "CREATE TABLE IF NOT EXISTS lifecycle_fixture (id INTEGER PRIMARY KEY)")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.WaitReady(ctx); err == nil {
		t.Fatal("closed database ready")
	}
	owner, err := identity.OpenRuntimeTransportPolicy(second.policyPath)
	if err != nil {
		t.Fatal("policy lease retained", err)
	}
	owner.Close()
}

// lifecycleWrite repeats an idempotent statement until it commits. Readiness
// does not wait for an election: the two members that restore the quorum may
// still be choosing a leader, or the old one may be stepping down, and a
// write that meets either fails with an error the caller is expected to
// retry.
func lifecycleWrite(t *testing.T, ctx context.Context, db *sql.DB, description, statement string) {
	t.Helper()
	for {
		_, err := db.ExecContext(ctx, statement)
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v (last error: %v)", description, ctx.Err(), err)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// The upstream join marker can survive a committed Add. Verify the exact
// failure and the explicit, offline reconciliation that the product must own.
func TestDatabaseRetainedJoinMarkerRequiresExplicitReconciliation(t *testing.T) {
	requireDatabaseHelper(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	trust, policy, fixtures := authorizedDatabaseFixtures(t, 2)
	for _, f := range fixtures {
		if err := identity.PublishTransportPolicy(f.policyPath, 0, policy); err != nil {
			t.Fatal(err)
		}
	}
	a := openAuthorizedFixture(t, ctx, trust, fixtures[0])
	f := fixtures[1]
	f.config.Seeds = []string{a.Address}
	b := openAuthorizedFixture(t, ctx, trust, f)
	id := b.ID
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.config.Dir, "join")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := StartAuthorizedDatabase(ctx, f.config, f.policyPath, trust, f.certificate, f.key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if b.ID != id {
		t.Fatal("retained marker changed identity")
	}
	short, stop := context.WithTimeout(ctx, 1200*time.Millisecond)
	if err := b.WaitReady(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplicate Add unexpectedly ready: %v", err)
	}
	stop()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("join marker unexpectedly removed", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	members, err := a.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("unexpected membership %#v", members)
	}
	found := false
	for _, m := range members {
		if m.ID == id && m.Address == f.config.Address {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing exact membership %#v", members)
	}
	if err := ReconcileCommittedJoinMarker(ctx, f.config.Dir, nil); err == nil {
		t.Fatal("reconciled without the observed membership")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("rejected reconciliation removed the join marker", err)
	}
	if err := ReconcileCommittedJoinMarker(ctx, f.config.Dir, members); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed membership left the join marker", err)
	}
	b = openAuthorizedFixture(t, ctx, trust, f)
	if b.ID != id {
		t.Fatal("reconciliation changed identity")
	}
}
