package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentStartTimeoutMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO deployments (id, name) VALUES (1, 'old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 132)

	db := open("deployment-start-timeout")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "StartTimeoutSec") {
		t.Fatal("start_timeout_sec was not added")
	}
	// An existing deployment states no budget of its own: it keeps following
	// the cluster default.
	var stated int64
	if err := db.Raw("SELECT COUNT(*) FROM deployments WHERE start_timeout_sec <> 0").Scan(&stated).Error; err != nil || stated != 0 {
		t.Fatalf("existing rows with a budget = %d err=%v", stated, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 132).Error; err != nil || marker.Name != "deployment_start_timeout" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 132").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("deployment-start-timeout-rollback")
	want := errors.New("injected start_timeout_sec failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "StartTimeoutSec") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 132).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	// The next start applies it cleanly.
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Deployment{}, "StartTimeoutSec") {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}
