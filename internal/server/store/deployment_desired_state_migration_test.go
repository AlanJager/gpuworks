package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentDesiredStateMigrationRenamesColumnAndPreservesRows(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:deployment-desired-state-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, served_name TEXT, state TEXT, name TEXT)`,
		`INSERT INTO deployments(id, served_name, state, name) VALUES(7, 'qwen', 'active', 'Qwen')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	m := migrationByVersion(t, 74)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasColumn("deployments", "state") || !db.Migrator().HasColumn("deployments", "desired_state") {
		t.Fatal("deployment state column was not renamed")
	}
	var value string
	if err := db.Raw(`SELECT desired_state FROM deployments WHERE id = 7`).Scan(&value).Error; err != nil {
		t.Fatal(err)
	}
	if value != "active" {
		t.Fatalf("desired_state=%q, want active", value)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 74).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
}

func TestDeploymentDesiredStateMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:deployment-desired-state-migration-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, state TEXT)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("injected deployment state migration failure")
	err = applyMigration(db, migration{Version: 9074, Name: "deployment-state-failure", Apply: func(tx *gorm.DB) error {
		if err := renameDeploymentStateColumn(tx); err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if !db.Migrator().HasColumn("deployments", "state") || db.Migrator().HasColumn("deployments", "desired_state") {
		t.Fatal("failed migration did not roll back column rename")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9074).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
}
