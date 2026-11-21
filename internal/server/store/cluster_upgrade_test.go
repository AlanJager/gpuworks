package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// upgradeFixture is a store at the executable's schema with three admitted
// members, and a longer migration list standing for a newer executable.
type upgradeFixture struct {
	st   *Store
	keys []string
	next clusterUpgrader
	// failSecond makes the newer list's second migration fail once.
	failSecond *bool
}

func newUpgradeFixture(t *testing.T) upgradeFixture {
	t.Helper()
	st := newTestStore(t)
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
	fail := false
	head := migrations[len(migrations)-1].Version
	list := append(append([]migration{}, migrations...),
		migration{Version: head + 1, Name: "upgrade_fixture_one", Apply: func(tx *gorm.DB) error {
			return tx.Exec(`CREATE TABLE upgrade_fixture_one (id INTEGER PRIMARY KEY)`).Error
		}},
		migration{Version: head + 2, Name: "upgrade_fixture_two", Apply: func(tx *gorm.DB) error {
			if err := tx.Exec(`CREATE TABLE upgrade_fixture_two (id INTEGER PRIMARY KEY)`).Error; err != nil {
				return err
			}
			if fail {
				fail = false
				return errors.New("injected failure after the table was created")
			}
			return nil
		}})
	return upgradeFixture{st: st, keys: keys, next: clusterUpgrader{s: st, list: list}, failSecond: &fail}
}

func (f upgradeFixture) reportAll(t *testing.T) {
	t.Helper()
	for _, key := range f.keys {
		if err := f.next.report(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
}

func tableExists(t *testing.T, st *Store, name string) bool {
	t.Helper()
	var count int
	if err := st.db.Raw(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func TestPendingMigrationsRequireAPrefix(t *testing.T) {
	f := newUpgradeFixture(t)
	if pending, err := pendingSchemaMigrations(f.st.db); err != nil || pending != 0 {
		t.Fatalf("current schema: pending = %d, %v; want 0", pending, err)
	}
	if pending, err := pendingMigrationsOf(f.st.db, f.next.list); err != nil || pending != 2 {
		t.Fatalf("newer executable: pending = %d, %v; want 2", pending, err)
	}
	if _, err := pendingMigrationsOf(f.st.db, migrations[:len(migrations)-1]); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("older executable: %v; want ErrSchemaIncompatible", err)
	}
	renamed := append([]migration{}, migrations...)
	renamed[len(renamed)-1].Name = "another_branch"
	if _, err := pendingMigrationsOf(f.st.db, renamed); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("diverged executable: %v; want ErrSchemaIncompatible", err)
	}
}

func TestBeginRequiresMigrationsAndEveryMember(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t)
	if _, err := f.st.BeginClusterUpgrade(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("begin with nothing to migrate: %v; want a refusal", err)
	}
	if _, err := f.next.begin(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("begin without reports: %v; want a refusal", err)
	}
	for _, key := range f.keys[:2] {
		if err := f.next.report(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	// The third member runs the executable the database is at.
	if err := f.st.ReportClusterUpgradeCapability(ctx, f.keys[2]); err != nil {
		t.Fatal(err)
	}
	status, err := f.next.begin(ctx)
	if !errors.Is(err, ErrClusterUpgradeRefused) || status.Ready() {
		t.Fatalf("begin with one member on another executable: ready=%v, %v; want a refusal", status.Ready(), err)
	}
	phase, err := f.st.ReadMaintenancePhase(ctx)
	if err != nil || phase.Phase != MaintenanceServing || phase.Revision != 1 {
		t.Fatalf("refused begins moved the phase to %+v, %v", phase, err)
	}
	if status.Base != 0 {
		t.Fatalf("a refused begin recorded base %d", status.Base)
	}
}

func TestUpgradeWindowAppliesMigrationsOnceAndReopens(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t)
	f.reportAll(t)
	applied := len(migrations)

	status, err := f.next.begin(ctx)
	if err != nil || status.Phase.Phase != MaintenanceUpgrading || status.Base != applied {
		t.Fatalf("begin = %+v, %v; want upgrading with base %d", status.Phase, err, applied)
	}
	if again, err := f.next.begin(ctx); err != nil || again.Phase != status.Phase {
		t.Fatalf("a repeated begin = %+v, %v; want the same phase %+v", again.Phase, err, status.Phase)
	}
	if err := f.st.SetBusinessSetting(ctx, "orphan_cleanup", "true"); !errors.Is(err, ErrBusinessWritesClosed) {
		t.Fatalf("business write in the window: %v; want ErrBusinessWritesClosed", err)
	}
	if _, err := f.next.finish(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("finish before migrating: %v; want a refusal", err)
	}

	*f.failSecond = true
	status, err = f.next.migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("migrate with an injected failure: %v", err)
	}
	status, err = f.next.status(ctx)
	if err != nil || status.Applied != applied+1 {
		t.Fatalf("after the failure applied = %d, %v; want %d", status.Applied, err, applied+1)
	}
	if !tableExists(t, f.st, "upgrade_fixture_one") || tableExists(t, f.st, "upgrade_fixture_two") {
		t.Fatal("the failed migration left its table, or the one before it is missing")
	}
	if _, err := f.next.abort(ctx); !errors.Is(err, ErrClusterUpgradeRefused) {
		t.Fatalf("abort after a migration applied: %v; want a refusal", err)
	}

	status, err = f.next.migrate(ctx)
	if err != nil || status.Pending() != 0 || !tableExists(t, f.st, "upgrade_fixture_two") {
		t.Fatalf("repeated migrate = pending %d, %v; want every migration applied", status.Pending(), err)
	}
	if again, err := f.next.migrate(ctx); err != nil || again.Applied != status.Applied {
		t.Fatalf("migrate with nothing left = %d, %v", again.Applied, err)
	}

	status, err = f.next.finish(ctx)
	if err != nil || status.Phase.Phase != MaintenanceServing || status.Base != 0 {
		t.Fatalf("finish = %+v base %d, %v; want serving", status.Phase, status.Base, err)
	}
	var left int
	if err := f.st.db.Raw(`SELECT count(*) FROM settings WHERE key LIKE 'cluster.upgrade.%'`).Row().Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d upgrade records left after finish, %v", left, err)
	}
	if err := f.st.SetBusinessSetting(ctx, "orphan_cleanup", "true"); err != nil {
		t.Fatalf("business write after finish: %v", err)
	}
	if again, err := f.next.finish(ctx); err != nil || again.Phase != status.Phase {
		t.Fatalf("a repeated finish = %+v, %v", again.Phase, err)
	}
}

func TestAbortReturnsToServingOnlyWithoutASchemaChange(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t)
	f.reportAll(t)
	if _, err := f.next.begin(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := f.next.abort(ctx)
	if err != nil || status.Phase.Phase != MaintenanceServing {
		t.Fatalf("abort before migrating = %+v, %v; want serving", status.Phase, err)
	}
	if after, err := f.next.status(ctx); err != nil || after.Base != 0 || after.Ready() {
		t.Fatalf("abort left base %d ready %v, %v", after.Base, after.Ready(), err)
	}
	if err := f.st.SetBusinessSetting(ctx, "orphan_cleanup", "true"); err != nil {
		t.Fatalf("business write after abort: %v", err)
	}

	// A begin that stopped at freezing is aborted from there.
	f.reportAll(t)
	phase, err := f.st.ReadMaintenancePhase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.TransitionMaintenancePhase(ctx, phase, MaintenanceFreezing); err != nil {
		t.Fatal(err)
	}
	if status, err := f.next.abort(ctx); err != nil || status.Phase.Phase != MaintenanceServing {
		t.Fatalf("abort from freezing = %+v, %v; want serving", status.Phase, err)
	}
}

func TestUpgradeRecordsAreNotPlatformSettings(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t)
	f.reportAll(t)
	listed, err := f.st.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{clusterUpgradeBaseSetting, clusterUpgradeMemberSetting + f.keys[0][:40]} {
		if _, ok := listed[key]; ok {
			t.Errorf("AllSettings lists the upgrade record %q", key)
		}
		if err := f.st.SetBusinessSetting(ctx, SettingKey(key), `{"markers":1,"last":"x"}`); !errors.Is(err, ErrInternalSchedulingSetting) {
			t.Errorf("client write to %q = %v; want ErrInternalSchedulingSetting", key, err)
		}
	}
	if err := f.st.ReportClusterUpgradeCapability(ctx, "short"); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("report under an invalid key: %v", err)
	}
}

// A schema behind the executable is its own class: an observer reads it, a
// member that would own the store is told to wait, and an executable behind
// the schema is still refused.
func TestJoinedSchemaBehindTheExecutableWaits(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	expected := ClusterIdentity{ID: 1, ClusterID: strings.Repeat("d", 32), CAFingerprint: strings.Repeat("e", 64), Protocol: ClusterProtocolVersion}
	if err := st.db.Create(&expected).Error; err != nil {
		t.Fatal(err)
	}
	if class, err := classifyJoinedSchemaClass(ctx, st.db, expected); err != nil || class != joinedSchemaCurrent {
		t.Fatalf("current schema: class %d, %v", class, err)
	}
	head := migrations[len(migrations)-1]
	if err := st.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, head.Version).Error; err != nil {
		t.Fatal(err)
	}
	if class, err := classifyJoinedSchemaClass(ctx, st.db, expected); err != nil || class != joinedSchemaPending {
		t.Fatalf("schema one migration behind: class %d, %v; want pending", class, err)
	}
	if legacy, err := classifyJoinedSchema(ctx, st.db, expected); !errors.Is(err, ErrSchemaUpgradePending) || legacy {
		t.Fatalf("owner of a schema behind: legacy %v, %v; want ErrSchemaUpgradePending", legacy, err)
	}
	if err := st.db.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, CURRENT_TIMESTAMP), (?, 'from_a_newer_executable', CURRENT_TIMESTAMP)`,
		head.Version, head.Name, head.Version+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := classifyJoinedSchemaClass(ctx, st.db, expected); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("schema ahead of the executable: %v; want ErrSchemaIncompatible", err)
	}
}
