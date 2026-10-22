package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecShmBytesMigration(t *testing.T) {
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
	m := migrationByVersion(t, 167)
	if m.Name != "inference_spec_shm_bytes" {
		t.Fatalf("migration 167 = %q", m.Name)
	}

	db := open("spec-shm-bytes-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var shm int64
	if err := db.Raw("SELECT shm_bytes FROM inference_specs WHERE id = 1").Scan(&shm).Error; err != nil || shm != 0 {
		t.Fatalf("existing spec shm_bytes = %d err=%v", shm, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 167).Error; err != nil || marker.Name != "inference_spec_shm_bytes" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("spec-shm-bytes-migration-rollback")
	want := errors.New("injected spec shm_bytes failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&InferenceSpecRow{}, "ShmBytes") {
		t.Fatal("shm_bytes survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 167).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "ShmBytes") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
