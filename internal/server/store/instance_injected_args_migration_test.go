package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDropInstanceInjectedArgsMigrationRemovesColumnIdempotently(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-injected-args-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, spec TEXT, injected_args TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO instances(id, spec, injected_args) VALUES (1, '{}', '["--host 127.0.0.1"]')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 90)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasColumn("instances", "injected_args") {
		t.Fatal("injected_args column survived the migration")
	}
	var spec string
	if err := db.Raw(`SELECT spec FROM instances WHERE id = 1`).Scan(&spec).Error; err != nil || spec != "{}" {
		t.Fatalf("row survived = %q err=%v", spec, err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 90).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if markers != 1 {
		t.Fatalf("rerun changed marker count to %d", markers)
	}
}

func TestDropInstanceInjectedArgsRollsBackSchemaAndMarkerOnFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-injected-args-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE instances (id INTEGER PRIMARY KEY, spec TEXT, injected_args TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected failure after the drop")
	err = applyMigration(db, migration{Version: 90, Name: "drop_instance_injected_args", Apply: func(tx *gorm.DB) error {
		if err := dropInstanceInjectedArgs(tx); err != nil {
			return err
		}
		return boom
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("applyMigration error=%v, want %v", err, boom)
	}
	if !db.Migrator().HasColumn("instances", "injected_args") {
		t.Fatal("failed migration dropped the column outside the transaction")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 90).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration left markers: count=%d err=%v", markers, err)
	}
}
