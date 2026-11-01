package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAPIKeyScopeMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		// The table as it was before keys could be scoped.
		if err := db.Exec(`CREATE TABLE api_keys (id INTEGER PRIMARY KEY, name TEXT, prefix TEXT UNIQUE, hash TEXT, user_id INTEGER, created_at DATETIME, last_used DATETIME, revoked_at DATETIME)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO api_keys (id, name, prefix, hash, user_id, created_at) VALUES (1, 'old', 'gw-old000000', 'h', 0, '2026-10-01 00:00:00')`).Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 193)
	if m.Name != "api_key_scope" {
		t.Fatalf("migration 193 = %s", m.Name)
	}
	applied := func(db *gorm.DB) bool {
		return db.Migrator().HasColumn(&APIKey{}, "Restricted") && db.Migrator().HasTable(&APIKeyDeployment{})
	}

	db := open("api-key-scope")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !applied(db) {
		t.Fatal("restricted column or grant table missing")
	}
	// An existing key keeps reaching every deployment.
	var key APIKey
	if err := db.First(&key, 1).Error; err != nil || key.Restricted || key.Name != "old" {
		t.Fatalf("existing key = %+v err=%v", key, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 193).Error; err != nil || marker.Name != "api_key_scope" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 193").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("api-key-scope-rollback")
	want := errors.New("injected api key scope failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&APIKey{}, "Restricted") || rollback.Migrator().HasTable(&APIKeyDeployment{}) {
		t.Fatal("the schema change survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 193).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !applied(rollback) {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	empty, err := gorm.Open(sqlite.Open("file:api-key-scope-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&APIKey{}) || empty.Migrator().HasTable(&APIKeyDeployment{}) {
		t.Fatalf("without api_keys: err=%v", err)
	}
}
