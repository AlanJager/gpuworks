package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecMemlockUnlimitedMigration(t *testing.T) {
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
	m := migrationByVersion(t, 168)
	if m.Name != "inference_spec_memlock_unlimited" {
		t.Fatalf("migration 168 = %q", m.Name)
	}

	db := open("spec-memlock-unlimited-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var flag bool
	if err := db.Raw("SELECT memlock_unlimited FROM inference_specs WHERE id = 1").Scan(&flag).Error; err != nil || flag {
		t.Fatalf("existing spec memlock_unlimited = %v err=%v", flag, err)
	}
	var null int64
	if err := db.Raw("SELECT COUNT(*) FROM inference_specs WHERE memlock_unlimited IS NULL").Scan(&null).Error; err != nil || null != 0 {
		t.Fatalf("existing specs left NULL = %d err=%v", null, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 168).Error; err != nil || marker.Name != "inference_spec_memlock_unlimited" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("spec-memlock-unlimited-migration-rollback")
	want := errors.New("injected spec memlock_unlimited failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&InferenceSpecRow{}, "MemlockUnlimited") {
		t.Fatal("memlock_unlimited survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 168).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "MemlockUnlimited") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
