package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// admitUpgradeMembers gives a store three admitted members.
func admitUpgradeMembers(t *testing.T, st *Store) []string {
	t.Helper()
	keys := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i, key := range keys {
		invitation := ""
		if i > 0 {
			invitation = strings.Repeat(string(rune('1'+i)), 64)
		}
		if err := st.db.Exec(`INSERT INTO cluster_admissions (key_fingerprint, invitation_hash, address, enrollment_origin, admitted_at, material_grant_sequence)
			VALUES (?, ?, ?, '', ?, 0)`, key, invitation, "192.0.2."+string(rune('1'+i))+":9001", time.Now().UTC()).Error; err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// The window applies an extension module's new step and a module the earlier
// executable did not have, once each, after a failure part way; and the
// core schema did not have to change for the window to be needed.
func TestUpgradeWindowAppliesExtensionModuleMigrations(t *testing.T) {
	ctx := context.Background()
	st := mustOpenWithSets(t, filepath.Join(t.TempDir(), "gpuworks.db"), quotaSet())
	keys := admitUpgradeMembers(t, st)

	fail := true
	grown := quotaSet()
	grown.Migrations = append(grown.Migrations, createStep(3, "limits", "CREATE TABLE ee_quota_limits (id INTEGER PRIMARY KEY)"))
	added := MigrationSet{Namespace: "audit", TablePrefix: "ee_audit_", Migrations: []Migration{
		{Version: 1, Name: "events", Apply: func(tx *gorm.DB) error {
			if err := tx.Exec("CREATE TABLE ee_audit_events (id INTEGER PRIMARY KEY)").Error; err != nil {
				return err
			}
			if fail {
				fail = false
				return errors.New("injected failure after the table was created")
			}
			return nil
		}},
	}}
	next := st.ClusterUpgradeFor([]MigrationSet{grown, added})

	status, err := next.Status(ctx)
	if err != nil || status.Pending() != 2 || status.Known != status.Applied || status.KnownExtensions != 4 || status.AppliedExtensions != 2 {
		t.Fatalf("status = %+v, %v; want two pending extension steps and no core step", status, err)
	}
	for _, key := range keys {
		if err := next.Report(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if status, err = next.Begin(ctx); err != nil || status.Phase.Phase != MaintenanceUpgrading {
		t.Fatalf("begin = %+v, %v", status.Phase, err)
	}
	if _, err := next.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("migrate with an injected failure: %v", err)
	}
	if got := markers(t, st, "quota"); len(got) != 3 {
		t.Fatalf("quota steps after the failure = %d; want the new one applied", len(got))
	}
	if got := markers(t, st, "audit"); len(got) != 0 || tableExists(t, st, "ee_audit_events") {
		t.Fatalf("the failed step left %d markers or its table", len(got))
	}
	if _, err := next.Abort(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("abort after an extension step applied: %v; want a refusal", err)
	}
	if _, err := next.Finish(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("finish with a step pending: %v; want a refusal", err)
	}
	if status, err = next.Migrate(ctx); err != nil || status.Pending() != 0 {
		t.Fatalf("repeated migrate = pending %d, %v", status.Pending(), err)
	}
	if got := markers(t, st, "audit"); len(got) != 1 || got[0].Name != "events" {
		t.Fatalf("audit steps = %+v; want the one step once", got)
	}
	if status, err = next.Finish(ctx); err != nil || status.Phase.Phase != MaintenanceServing {
		t.Fatalf("finish = %+v, %v", status.Phase, err)
	}
	if err := st.AdoptExtensionSchemas(ctx, []MigrationSet{grown, added}); err != nil {
		t.Fatalf("the upgraded executable adopts its schemas: %v", err)
	}
}

// An executable without a module the database holds takes no part in an
// upgrade and is not adopted as a cluster member's schema.
func TestExecutableLackingAModuleIsRefused(t *testing.T) {
	ctx := context.Background()
	st := mustOpenWithSets(t, filepath.Join(t.TempDir(), "gpuworks.db"), quotaSet())
	admitUpgradeMembers(t, st)
	if _, err := st.ClusterUpgradeFor(nil).Status(ctx); !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("status of an executable without the module: %v", err)
	}
	if err := st.AdoptExtensionSchemas(ctx, nil); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("adopt without the module: %v; want ErrSchemaIncompatible", err)
	}
	shorter := quotaSet()
	shorter.Migrations = shorter.Migrations[:1]
	if _, err := st.ClusterUpgradeFor([]MigrationSet{shorter}).Status(ctx); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("status of an executable behind the module's schema: %v", err)
	}
}

// A member that came back on another executable has replaced its report, and
// reports that are no longer repeated count only while most members' are.
func TestUpgradeStepsFollowTheMembersLastReports(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t)
	f.reportAll(t)
	if _, err := f.next.begin(ctx); err != nil {
		t.Fatal(err)
	}
	// One member is put back on the executable the database is at.
	if err := f.st.ReportClusterUpgradeCapability(ctx, f.keys[1]); err != nil {
		t.Fatal(err)
	}
	if status, err := f.next.migrate(ctx); !errors.Is(err, ErrClusterUpgradeRefused) || !strings.Contains(err.Error(), "192.0.2.2:9001") || status.Pending() != 2 {
		t.Fatalf("migrate with a member on the earlier executable: pending %d, %v", status.Pending(), err)
	}
	if err := f.next.report(ctx, f.keys[1]); err != nil {
		t.Fatal(err)
	}
	age := func(key string, seconds int) {
		t.Helper()
		setting, err := clusterUpgradeMemberKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.st.db.Exec(`UPDATE settings SET updated_at = datetime('now', ?) WHERE key = ?`, fmt.Sprintf("-%d seconds", seconds), setting).Error; err != nil {
			t.Fatal(err)
		}
	}
	// One member is down: its report stays and the others carry the step.
	age(f.keys[2], 120)
	status, err := f.next.status(ctx)
	if err != nil || !status.Ready() || status.Members[2].Fresh {
		t.Fatalf("one silent member: ready %v fresh %v, %v", status.Ready(), status.Members[2].Fresh, err)
	}
	age(f.keys[0], 120)
	if _, err := f.next.migrate(ctx); !errors.Is(err, ErrClusterUpgradeRefused) || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("migrate with two silent members: %v", err)
	}
	// A repeated report makes the member present again.
	if err := f.next.report(ctx, f.keys[0]); err != nil {
		t.Fatal(err)
	}
	if status, err := f.next.migrate(ctx); err != nil || status.Pending() != 0 {
		t.Fatalf("migrate after the report was repeated: pending %d, %v", status.Pending(), err)
	}
}
