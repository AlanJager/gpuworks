package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecDirsMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO inference_specs (id, name) VALUES (1, 'bf16')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 230)
	if m.Name != "inference_spec_dirs" {
		t.Fatalf("migration 230 = %q", m.Name)
	}

	db := open("spec-dirs-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	// A spec stored before the column declares no directories: it reads back
	// empty, not NULL, which deploy.DecodeManagedDirs takes as none.
	var null int64
	if err := db.Raw("SELECT COUNT(*) FROM inference_specs WHERE dirs IS NULL OR dirs <> ''").Scan(&null).Error; err != nil || null != 0 {
		t.Fatalf("existing specs not empty = %d err=%v", null, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 230).Error; err != nil || marker.Name != "inference_spec_dirs" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-apply on a migrated table: %v", err)
	}

	rollback := open("spec-dirs-migration-rollback")
	want := errors.New("injected spec dirs failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&InferenceSpecRow{}, "Dirs") {
		t.Fatal("dirs survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 230).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "Dirs") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
