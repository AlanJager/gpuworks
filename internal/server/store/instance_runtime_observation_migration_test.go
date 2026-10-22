package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInstanceRuntimeObservationMigrationAddsColumnsAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:runtime-observation-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 92)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"runtime_observation", "runtime_observation_received_at"} {
		if !db.Migrator().HasColumn("instances", column) {
			t.Fatalf("missing column %s", column)
		}
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("migration body is not idempotent: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 92).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
}

func TestInstanceRuntimeObservationMigrationRollsBackColumnsAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:runtime-observation-migration-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected runtime observation migration failure")
	base := migrationByVersion(t, 92)
	failing := migration{Version: 9092, Name: "runtime-observation-failure", Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	for _, column := range []string{"runtime_observation", "runtime_observation_received_at"} {
		if db.Migrator().HasColumn("instances", column) {
			t.Fatalf("failed migration left column %s", column)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", failing.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
}

func TestInstanceRuntimeObservationMigrationRollsBackWhenMarkerInsertFailsAndRetries(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:runtime-observation-marker-failure?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_runtime_observation_marker BEFORE INSERT ON schema_migrations
		WHEN NEW.version = 92 BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 92)
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("marker failure unexpectedly succeeded")
	}
	for _, column := range []string{"runtime_observation", "runtime_observation_received_at"} {
		if db.Migrator().HasColumn("instances", column) {
			t.Fatalf("marker failure left column %s", column)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 92).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_runtime_observation_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after transactional rollback: %v", err)
	}
	for _, column := range []string{"runtime_observation", "runtime_observation_received_at"} {
		if !db.Migrator().HasColumn("instances", column) {
			t.Fatalf("successful retry missing column %s", column)
		}
	}
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 92).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("retry marker count=%d err=%v", markers, err)
	}
}
