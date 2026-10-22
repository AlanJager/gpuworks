package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDropDeploymentRestartCountMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, restart_count INTEGER)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO deployments (id, name, restart_count) VALUES (1, 'group', 2)").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	hasColumn := func(db *gorm.DB) bool {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM pragma_table_info('deployments') WHERE name = 'restart_count'").Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	m := migrationByVersion(t, 110)

	db := open("drop-restart-count")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if hasColumn(db) {
		t.Fatal("restart_count survived the migration")
	}
	var name string
	if err := db.Raw("SELECT name FROM deployments WHERE id = 1").Scan(&name).Error; err != nil || name != "group" {
		t.Fatalf("existing deployment = %q err=%v", name, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 110).Error; err != nil || marker.Name != "drop_deployment_restart_count" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("drop-restart-count-rollback")
	want := errors.New("injected restart_count failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if !hasColumn(rollback) {
		t.Fatal("restart_count was dropped by the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 110).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || hasColumn(rollback) {
		t.Fatalf("restart after rollback: %v", err)
	}
}
