package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// openSchedulingCenterLegacy is a database from before the scheduling center:
// deployments and instances without its columns, one row each, and no
// schedule table.
func openSchedulingCenterLegacy(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, desired_state TEXT, sleepable NUMERIC, replicas INTEGER)`,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT, deployment_id INTEGER, state TEXT, sleep_level INTEGER)`,
		`INSERT INTO deployments (id, name, desired_state, sleepable, replicas) VALUES (1, 'old', 'active', 1, 2)`,
		`INSERT INTO instances (id, uuid, deployment_id, state, sleep_level) VALUES (1, 'old-1', 1, 'running', 2)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestSchedulingCenterMigrations(t *testing.T) {
	ms := []migration{migrationByVersion(t, 182), migrationByVersion(t, 183), migrationByVersion(t, 184)}
	db := openSchedulingCenterLegacy(t, "scheduling-center")
	if err := migrateDB(db, ms); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "WakeWaitingSince") || !db.Migrator().HasIndex(&Instance{}, "WakeWaitingSince") {
		t.Fatal("instances.wake_waiting_since missing or unindexed")
	}
	for _, column := range []string{"IdleSleepAfterSec", "IdleSleepLevel"} {
		if !db.Migrator().HasColumn(&Deployment{}, column) {
			t.Fatalf("deployments.%s missing", column)
		}
	}
	if !db.Migrator().HasTable(&LifecycleSchedule{}) || !db.Migrator().HasIndex(&LifecycleSchedule{}, "DeploymentID") {
		t.Fatal("lifecycle_schedules missing or unindexed")
	}
	// Existing rows read as before the scheduling center: nothing waiting and
	// idle sleep off.
	var inst struct{ WakeWaitingSince *string }
	if err := db.Raw(`SELECT wake_waiting_since FROM instances WHERE id = 1`).Scan(&inst).Error; err != nil || inst.WakeWaitingSince != nil {
		t.Fatalf("existing instance = %+v err=%v", inst, err)
	}
	var dep struct {
		IdleSleepAfterSec int
		IdleSleepLevel    int
	}
	if err := db.Raw(`SELECT idle_sleep_after_sec, idle_sleep_level FROM deployments WHERE id = 1`).Scan(&dep).Error; err != nil ||
		dep.IdleSleepAfterSec != 0 || dep.IdleSleepLevel != 2 {
		t.Fatalf("existing deployment = %+v err=%v", dep, err)
	}
	for _, version := range []int{182, 183, 184} {
		var marker SchemaMigration
		if err := db.First(&marker, version).Error; err != nil {
			t.Fatalf("marker %d: %v", version, err)
		}
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version IN (182, 183, 184)").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, ms); err != nil {
		t.Fatalf("rerun without markers: %v", err)
	}

	// A failure inside each migration leaves neither its schema nor its marker.
	for _, m := range ms {
		rollback := openSchedulingCenterLegacy(t, "scheduling-center-rollback-"+m.Name)
		want := errors.New("injected " + m.Name + " failure")
		failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
			if err := m.Apply(tx); err != nil {
				return err
			}
			return want
		}}
		if err := applyMigration(rollback, failing); !errors.Is(err, want) {
			t.Fatalf("%s error = %v", m.Name, err)
		}
		switch m.Version {
		case 182:
			if rollback.Migrator().HasColumn(&Deployment{}, "IdleSleepAfterSec") {
				t.Fatal("deployment columns survived the failed migration")
			}
		case 183:
			if rollback.Migrator().HasColumn(&Instance{}, "WakeWaitingSince") {
				t.Fatal("instance column survived the failed migration")
			}
		case 184:
			if rollback.Migrator().HasTable(&LifecycleSchedule{}) {
				t.Fatal("lifecycle_schedules survived the failed migration")
			}
		}
		var count int64
		if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s marker count = %d err=%v", m.Name, count, err)
		}
		if err := migrateDB(rollback, ms); err != nil {
			t.Fatalf("restart after %s rollback: %v", m.Name, err)
		}
	}
}

func TestInstanceEventKindIndexMigration(t *testing.T) {
	m := migrationByVersion(t, 185)
	db, err := gorm.Open(sqlite.Open("file:event-kind-index?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &InstanceEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&InstanceEvent{}, "idx_instance_events_kind_id") {
		t.Fatal("instance_events(kind, id) is not indexed")
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 185").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	rollback, err := gorm.Open(sqlite.Open("file:event-kind-index-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback.AutoMigrate(&SchemaMigration{}, &InstanceEvent{}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected index failure")
	if err := applyMigration(rollback, migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasIndex(&InstanceEvent{}, "idx_instance_events_kind_id") {
		t.Fatal("the index survived the failed migration")
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasIndex(&InstanceEvent{}, "idx_instance_events_kind_id") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
