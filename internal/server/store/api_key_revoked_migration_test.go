package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAPIKeyRevokedAtMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		// The table as it was before keys were revoked in place.
		if err := db.Exec(`CREATE TABLE api_keys (id INTEGER PRIMARY KEY, name TEXT, prefix TEXT UNIQUE, hash TEXT, user_id INTEGER, created_at DATETIME, last_used DATETIME)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO api_keys (id, name, prefix, hash, user_id, created_at) VALUES (1, 'old', 'gw-old000000', 'h', 0, '2026-10-01 00:00:00')`).Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 169)

	db := open("api-key-revoked")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&APIKey{}, "RevokedAt") {
		t.Fatal("revoked_at was not added")
	}
	// An existing key is live.
	var key APIKey
	if err := db.First(&key, 1).Error; err != nil || key.RevokedAt != nil || key.Name != "old" {
		t.Fatalf("existing key = %+v err=%v", key, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 169).Error; err != nil || marker.Name != "api_key_revoked_at" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 169").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("api-key-revoked-rollback")
	want := errors.New("injected revoked_at failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&APIKey{}, "RevokedAt") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 169).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&APIKey{}, "RevokedAt") {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	empty, err := gorm.Open(sqlite.Open("file:api-key-revoked-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&APIKey{}) {
		t.Fatalf("without api_keys: err=%v", err)
	}
}
