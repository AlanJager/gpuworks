package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestImageNodeRuntimeSizeMigration(t *testing.T) {
	closeAfterTest := func(db *gorm.DB) {
		t.Helper()
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := sqlDB.Close(); err != nil {
				t.Error(err)
			}
		})
	}

	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		closeAfterTest(db)
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`CREATE TABLE image_nodes (id INTEGER PRIMARY KEY, size_bytes INTEGER)`).Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 175)

	db := open("image-runtime-size")
	if err := db.Exec("INSERT INTO image_nodes (size_bytes) VALUES (4096)").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&ImageNode{}, "RuntimeSizeBytes") {
		t.Fatal("image_node_runtime_size was not added")
	}
	var legacy ImageNode
	if err := db.First(&legacy).Error; err != nil || legacy.RuntimeSizeBytes != nil {
		t.Fatalf("legacy runtime size = %v, err=%v; want unknown", legacy.RuntimeSizeBytes, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 175).Error; err != nil || marker.Name != "image_node_runtime_size" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 175").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("image-runtime-size-rollback")
	want := errors.New("injected image_node_runtime_size failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&ImageNode{}, "RuntimeSizeBytes") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 175).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&ImageNode{}, "RuntimeSizeBytes") {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	empty, err := gorm.Open(sqlite.Open("file:image-runtime-size-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	closeAfterTest(empty)
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&ImageNode{}) {
		t.Fatalf("without image_nodes: err=%v", err)
	}
}
