package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInstanceDesiredStateLaunchSnapshotMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:instance_desired_state_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (
		id INTEGER PRIMARY KEY,
		uuid TEXT,
		spec TEXT,
		run_spec_overrides TEXT
	)`).Error; err != nil {
		t.Fatal(err)
	}

	m := migrationByVersion(t, 70)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	// Version 70 brings instances up to the instance entity; the snapshot
	// column it once added is retired by version 111 and is no longer part of
	// the entity, so its schedule record stands in for "the entity's columns".
	if !db.Migrator().HasColumn(&Instance{}, "ScheduleExpl") {
		t.Fatal("instances were not brought up to the entity's columns")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 70).Error; err != nil || marker.Name != "instance_desired_state_launch_snapshot" {
		t.Fatalf("migration marker = %+v, err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestInstanceDesiredStateLaunchSnapshotMigrationRollsBack(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:instance_desired_state_migration_rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE instances (id INTEGER PRIMARY KEY, spec TEXT)").Error; err != nil {
		t.Fatal(err)
	}

	want := errors.New("injected instance migration failure")
	failing := migration{
		Version: 70,
		Name:    "instance_desired_state_launch_snapshot",
		Apply: func(tx *gorm.DB) error {
			if err := tx.Exec("ALTER TABLE instances ADD COLUMN image_defaults_snapshot TEXT").Error; err != nil {
				return err
			}
			return want
		},
	}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("migration error = %v, want %v", err, want)
	}
	if db.Migrator().HasColumn("instances", "image_defaults_snapshot") {
		t.Fatal("failed migration left image_defaults_snapshot behind")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 70).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 70)}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func migrationByVersion(t *testing.T, version int) migration {
	t.Helper()
	for _, item := range migrations {
		if item.Version == version {
			return item
		}
	}
	t.Fatalf("migration %d not found", version)
	return migration{}
}

// migrationByName finds a migration by its stable name. A version number says
// when a migration runs relative to every other one, so it moves whenever
// another migration lands ahead of it; the name does not.
func migrationByName(t *testing.T, name string) migration {
	t.Helper()
	for _, item := range migrations {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("migration %q not found", name)
	return migration{}
}
