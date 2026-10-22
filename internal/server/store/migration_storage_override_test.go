package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func storageOverrideLegacyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.Exec("CREATE TABLE storage_nodes (id INTEGER PRIMARY KEY, storage_id INTEGER, node_id INTEGER, label_key TEXT, cache_max_bytes INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO storage_nodes (id,storage_id,node_id,label_key,cache_max_bytes) VALUES (1,2,3,'',0),(2,2,0,'pool',NULL)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStorageOverrideScopeMigrationPreservesLegacySelectors(t *testing.T) {
	db := storageOverrideLegacyDB(t)
	m := migration{Version: 160, Name: "storage_node_override_scope", Apply: migrateStorageNodeOverrideScope}
	for range 2 {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Apply(db); err != nil {
		t.Fatal(err)
	}
	var rows []StorageNode
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].OverrideOnly || rows[1].OverrideOnly || rows[0].CacheMaxBytes == nil || *rows[0].CacheMaxBytes != 0 || rows[1].LabelKey != "pool" {
		t.Fatalf("legacy values changed: %+v", rows)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 160).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("marker: %d %v", count, err)
	}
	if err := db.Exec("INSERT INTO storage_nodes (id,storage_id,node_id) VALUES (3,2,4)").Error; err != nil {
		t.Fatal(err)
	}
	var row StorageNode
	if err := db.First(&row, 3).Error; err != nil || row.OverrideOnly {
		t.Fatalf("default: %+v %v", row, err)
	}
}

func TestStorageOverrideScopeMigrationRollsBackAndRetries(t *testing.T) {
	db := storageOverrideLegacyDB(t)
	boom := errors.New("injected after adding column")
	m := migration{Version: 160, Name: "storage_node_override_scope", Apply: func(tx *gorm.DB) error {
		if err := migrateStorageNodeOverrideScope(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := migrateDB(db, []migration{m}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if db.Migrator().HasColumn(&StorageNode{}, "OverrideOnly") {
		t.Fatal("failed migration left column")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 160).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed marker: %d %v", count, err)
	}
	m.Apply = migrateStorageNodeOverrideScope
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&StorageNode{}, "OverrideOnly") {
		t.Fatal("retry did not add column")
	}
}
