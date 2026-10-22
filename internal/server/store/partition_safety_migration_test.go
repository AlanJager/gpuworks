package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestPartitionSafetyMigrationIsAtomicAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "partition-safety.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	var m migration
	for _, candidate := range migrations {
		if candidate.Name == "node_partition_safety_protocol" {
			m = candidate
			break
		}
	}
	if m.Version != 93 {
		t.Fatalf("partition safety migration version = %d", m.Version)
	}
	injected := errors.New("injected failure")
	failing := m
	failing.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return injected
	}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, injected) {
		t.Fatalf("migration failure = %v", err)
	}
	if db.Migrator().HasColumn("nodes", "partition_safety_protocol") {
		t.Fatal("partition safety column survived failed migration")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed migration marker count = %d, err = %v", count, err)
	}
	for range 2 {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasColumn("nodes", "partition_safety_protocol") {
		t.Fatal("partition safety column missing")
	}
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("migration marker count = %d, err = %v", count, err)
	}
}
