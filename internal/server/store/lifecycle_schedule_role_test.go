package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// openLifecycleScheduleLegacy is a database as migration 187 left it: orders
// written before a schedule could name a PD role.
func openLifecycleScheduleLegacy(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db := openSchedulingCenterLegacy(t, name)
	for _, stmt := range []string{
		`CREATE TABLE lifecycle_schedules (id INTEGER PRIMARY KEY, deployment_id INTEGER NOT NULL, action TEXT NOT NULL, weekdays INTEGER NOT NULL, minute INTEGER NOT NULL, timezone TEXT NOT NULL DEFAULT '', sleep_level INTEGER NOT NULL DEFAULT 0, replicas INTEGER NOT NULL DEFAULT 0, enabled NUMERIC NOT NULL, last_fired_at DATETIME, created_at DATETIME, updated_at DATETIME)`,
		`INSERT INTO lifecycle_schedules (id, deployment_id, action, weekdays, minute, replicas, enabled) VALUES (1, 1, 'replicas', 127, 600, 3, 1)`,
		`INSERT INTO lifecycle_schedules (id, deployment_id, action, weekdays, minute, sleep_level, enabled) VALUES (2, 1, 'sleep', 127, 1260, 2, 1)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestLifecycleScheduleRoleMigration(t *testing.T) {
	m := migrationByVersion(t, 188)
	ms := []migration{m}
	db := openLifecycleScheduleLegacy(t, "schedule-role")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&LifecycleSchedule{}, "Role") {
		t.Fatal("lifecycle_schedules.role missing")
	}
	// Existing orders scale no role, and keep what they said.
	var rows []LifecycleSchedule
	if err := db.Order("id").Find(&rows).Error; err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	if rows[0].Role != "" || rows[0].Replicas != 3 || rows[0].Action != ScheduleActionReplicas || rows[1].Role != "" || rows[1].SleepLevel != 2 {
		t.Fatalf("existing orders changed: %+v", rows)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 188).Error; err != nil {
		t.Fatalf("marker: %v", err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// A rerun without the marker (the column already there) changes nothing.
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 188").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := openLifecycleScheduleLegacy(t, "schedule-role-rollback")
	want := errors.New("injected failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&LifecycleSchedule{}, "Role") {
		t.Fatal("role column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = 188").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, ms); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if !rollback.Migrator().HasColumn(&LifecycleSchedule{}, "Role") {
		t.Fatal("role column missing after the restart")
	}
}

func TestLifecycleScheduleRoleRoundTrip(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeployment(&Deployment{Name: "pd", ServedName: "pd", State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	sc := &LifecycleSchedule{DeploymentID: dep.ID, Action: ScheduleActionReplicas, Weekdays: 127, Minute: 600, Replicas: 4, Role: "decode", Enabled: true}
	if err := st.CreateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	got, err := st.LifecycleSchedule(dep.ID, sc.ID)
	if err != nil || got.Role != "decode" {
		t.Fatalf("stored = %+v err=%v", got, err)
	}
	sc.Role = "prefill"
	if err := st.UpdateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.LifecycleSchedule(dep.ID, sc.ID); got.Role != "prefill" {
		t.Fatalf("edited = %+v", got)
	}
	rows, err := st.EnabledLifecycleSchedules()
	if err != nil || len(rows) != 1 || rows[0].Role != "prefill" {
		t.Fatalf("enabled = %+v err=%v", rows, err)
	}
}
