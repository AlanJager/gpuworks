package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecImageSourcesMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT, image TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO inference_specs (id, name, image) VALUES (1, 'bf16', 'repo:v1')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByName(t, "inference_spec_image_sources")

	db := open("spec-image-sources-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&InferenceSpecRow{}, "ImageSources") {
		t.Fatal("image_sources column was not added")
	}
	var sources string
	if err := db.Raw("SELECT image_sources FROM inference_specs WHERE id = 1").Scan(&sources).Error; err != nil || sources != "" {
		t.Fatalf("existing spec image_sources = %q err=%v, want the empty list", sources, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil || marker.Name != "inference_spec_image_sources" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// A second run finds the marker and skips; applying the body again finds the
	// column and leaves the stored value alone.
	if err := db.Exec("UPDATE inference_specs SET image_sources = '[\"swr.example.com/org/repo:v1\"]' WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("body is not idempotent: %v", err)
	}
	if err := db.Raw("SELECT image_sources FROM inference_specs WHERE id = 1").Scan(&sources).Error; err != nil || sources != `["swr.example.com/org/repo:v1"]` {
		t.Fatalf("re-run changed image_sources to %q err=%v", sources, err)
	}

	rollback := open("spec-image-sources-migration-rollback")
	want := errors.New("injected spec image_sources failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&InferenceSpecRow{}, "ImageSources") {
		t.Fatal("image_sources survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "ImageSources") {
		t.Fatalf("restart after rollback: %v", err)
	}
}

// A database that predates the catalog tables is not this migration's business:
// the table does not exist yet, and the full schema creates the column with it.
func TestInferenceSpecImageSourcesMigrationSkipsMissingTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:spec-image-sources-no-table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "inference_spec_image_sources")
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&InferenceSpecRow{}) {
		t.Fatal("the migration must not create the table")
	}
}
