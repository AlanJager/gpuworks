//go:build linux && dqlite

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// On the replicated database a migration that holds its transaction for
// several seconds commits, a failure after it leaves exactly the committed
// steps, and the repeated step carries on from there, for the core list and
// an extension module together.
func TestNativeUpgradeMigratesSlowlyAndResumes(t *testing.T) {
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := OpenRuntimeWith(ctx, filepath.Join(dir, "gpuworks.db"), bus.New(), OpenOptions{Extensions: []MigrationSet{quotaSet()}, Backend: ReplicatedBackend()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys := admitUpgradeMembers(t, st)

	const held = 8 * time.Second
	fail := true
	head := migrations[len(migrations)-1].Version
	list := append(append([]migration{}, migrations...),
		migration{Version: head + 1, Name: "upgrade_fixture_slow", Apply: func(tx *gorm.DB) error {
			if err := tx.Exec(`CREATE TABLE upgrade_fixture_slow (id INTEGER PRIMARY KEY)`).Error; err != nil {
				return err
			}
			time.Sleep(held)
			return tx.Exec(`INSERT INTO upgrade_fixture_slow (id) VALUES (1)`).Error
		}},
		migration{Version: head + 2, Name: "upgrade_fixture_failing", Apply: func(tx *gorm.DB) error {
			if err := tx.Exec(`CREATE TABLE upgrade_fixture_failing (id INTEGER PRIMARY KEY)`).Error; err != nil {
				return err
			}
			if fail {
				fail = false
				return errors.New("injected failure after the table was created")
			}
			return nil
		}})
	grown := quotaSet()
	grown.Migrations = append(grown.Migrations, createStep(3, "limits", "CREATE TABLE ee_quota_limits (id INTEGER PRIMARY KEY)"))
	next := clusterUpgrader{s: st, list: list, sets: []MigrationSet{grown}}
	for _, key := range keys {
		if err := next.report(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if status, err := next.begin(ctx); err != nil || status.Phase.Phase != MaintenanceUpgrading {
		t.Fatalf("begin = %+v, %v", status.Phase, err)
	}
	started := time.Now()
	if _, err := next.migrate(ctx); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("migrate = %v; want the injected failure of the second migration", err)
	}
	if took := time.Since(started); took < held {
		t.Fatalf("the slow migration returned after %s; it holds its transaction for %s", took, held)
	}
	status, err := next.status(ctx)
	if err != nil || status.Applied != len(migrations)+1 || status.AppliedExtensions != 2 {
		t.Fatalf("after the failure: core %d extension %d, %v; want the slow migration only", status.Applied, status.AppliedExtensions, err)
	}
	if !tableExists(t, st, "upgrade_fixture_slow") || tableExists(t, st, "upgrade_fixture_failing") {
		t.Fatal("the committed migration is missing, or the failed one left its table")
	}
	started = time.Now()
	if status, err = next.migrate(ctx); err != nil || status.Pending() != 0 {
		t.Fatalf("repeated migrate = pending %d, %v", status.Pending(), err)
	}
	if took := time.Since(started); took >= held {
		t.Fatalf("the repeated step took %s: it ran the committed slow migration again", took)
	}
	if got := markers(t, st, "quota"); len(got) != 3 {
		t.Fatalf("quota steps = %d, want 3", len(got))
	}
	if status, err = next.finish(ctx); err != nil || status.Phase.Phase != MaintenanceServing {
		t.Fatalf("finish = %+v, %v", status.Phase, err)
	}
	if err := st.CheckBusinessReads(ctx); err != nil {
		t.Fatalf("business reads after the upgrade: %v", err)
	}
}
