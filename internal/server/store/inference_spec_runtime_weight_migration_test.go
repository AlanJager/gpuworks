package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecRuntimeWeightMigrationIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:spec_runtime_weight_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, weight_bytes INTEGER NOT NULL DEFAULT 0)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	migration := migrationByVersion(t, 73)
	if err := applyMigration(db, migration); err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !db.Migrator().HasColumn(&InferenceSpecRow{}, "RuntimeWeightBytes") {
		t.Fatal("runtime_weight_bytes column was not added")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 73).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
}

func TestInferenceSpecRuntimeWeightMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:spec_runtime_weight_migration_rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, weight_bytes INTEGER NOT NULL DEFAULT 0)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("injected failure after runtime weight migration")
	base := migrationByVersion(t, 73)
	err = applyMigration(db, migration{Version: 9073, Name: "runtime-weight-failure", Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&InferenceSpecRow{}, "RuntimeWeightBytes") {
		t.Fatal("runtime_weight_bytes survived failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9073).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
}
