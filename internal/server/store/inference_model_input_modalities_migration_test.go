package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceModelInputModalitiesMigrationIsIdempotentAndTransactional(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:inference_model_input_modalities_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 75)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("inference_models", "input_modalities") {
		t.Fatal("input_modalities column missing")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 75).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}

	want := errors.New("injected input modalities migration failure")
	failing := migration{Version: 1075, Name: "input_modalities_failure", Apply: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE inference_models ADD COLUMN rollback_probe TEXT").Error; err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error=%v, want=%v", err, want)
	}
	if db.Migrator().HasColumn("inference_models", "rollback_probe") {
		t.Fatal("failed migration left schema behind")
	}
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 1075).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed marker count=%d err=%v", markers, err)
	}
}
