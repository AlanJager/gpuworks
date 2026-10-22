package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecSourceMigrationBackfillsIdentityAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:spec_source_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	modelDigest := strings.Repeat("a", 64)
	for _, statement := range []string{
		`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL, source TEXT NOT NULL, package_digest TEXT)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, engine TEXT, image TEXT, quantization TEXT, weight_sources TEXT, gpus TEXT)`,
		`CREATE UNIQUE INDEX idx_inference_spec_identity ON inference_specs(model_id, name)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY)`,
		`INSERT INTO inference_models(id, model_id, source, package_digest) VALUES(1, 'acme/model', 'upstream', '` + modelDigest + `')`,
		`INSERT INTO inference_specs(id, model_id, name, engine, image, quantization, weight_sources, gpus) VALUES(1, 1, 'general', 'vllm', 'image', 'bf16', '[]', '{}')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasIndex(&InferenceSpecRow{}, "idx_inference_spec_identity") {
		t.Fatal("v71 fixture omitted old spec identity index")
	}
	migration := migrationByVersion(t, 72)
	if err := migration.Apply(db); err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	var got InferenceSpecRow
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatal(err)
	}
	if got.Source != "upstream" || got.PackageDigest != modelDigest {
		t.Fatalf("backfilled spec=%+v", got)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "TemplateSpecSource") {
		t.Fatal("deployment source provenance column was not added")
	}
	user := got
	user.ID = 0
	user.Source = "user"
	user.Image = "user-image"
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("same name from user source was rejected: %v", err)
	}
	duplicate := got
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("same source/name duplicate was accepted")
	}
}

func TestInferenceSpecSourceMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:spec_source_migration_rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL, source TEXT NOT NULL, package_digest TEXT)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, engine TEXT, image TEXT, quantization TEXT, weight_sources TEXT, gpus TEXT)`,
		`CREATE UNIQUE INDEX idx_inference_spec_identity ON inference_specs(model_id, name)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("injected failure after spec source migration")
	base := migrationByVersion(t, 72)
	err = applyMigration(db, migration{Version: 9072, Name: "spec-source-failure", Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&InferenceSpecRow{}, "Source") || db.Migrator().HasColumn(&InferenceSpecRow{}, "PackageDigest") || db.Migrator().HasColumn(&Deployment{}, "TemplateSpecSource") {
		t.Fatal("source identity schema survived failed migration")
	}
	var indexSQL string
	if err := db.Raw(`SELECT sql FROM sqlite_schema WHERE type='index' AND name='idx_inference_spec_identity'`).Scan(&indexSQL).Error; err != nil || !strings.Contains(indexSQL, "model_id, name") {
		t.Fatalf("old identity index was not restored: sql=%q err=%v", indexSQL, err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9072).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
}

func TestInferenceSpecSourceMigrationPreservesHistoricalOwnership(t *testing.T) {
	for _, sourceColumn := range []string{"", ", source TEXT"} {
		t.Run(sourceColumn, func(t *testing.T) {
			db := historicalSchemaDB(t,
				`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL, source TEXT, package_digest TEXT)`,
				`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER NOT NULL, name TEXT NOT NULL, engine TEXT, image TEXT, weight_sources TEXT, gpus TEXT`+sourceColumn+`)`,
				`CREATE UNIQUE INDEX idx_inference_spec_identity ON inference_specs(model_id, name)`,
				`CREATE TABLE deployments (id INTEGER PRIMARY KEY)`,
				`INSERT INTO inference_models(id, model_id, source, package_digest) VALUES(1, 'acme/user', 'user', 'user-digest'), (2, 'acme/builtin', 'upstream', 'builtin-digest'), (3, 'acme/unknown', NULL, NULL)`,
				`INSERT INTO inference_specs(id, model_id, name, image, weight_sources, gpus) VALUES(1, 1, 'user-spec', 'image', '[]', '{}'), (2, 2, 'builtin-spec', 'image', '[]', '{}'), (3, 3, 'unknown-spec', 'image', '[]', '{}')`)
			m := migrationByVersion(t, 72)
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[uint][2]string{1: {"user", "user-digest"}, 2: {"upstream", "builtin-digest"}, 3: {"upstream", ""}} {
				var got InferenceSpecRow
				if err := db.First(&got, id).Error; err != nil {
					t.Fatal(err)
				}
				if got.Source != want[0] || got.PackageDigest != want[1] {
					t.Errorf("spec %d ownership=(%q, %q), want %v", id, got.Source, got.PackageDigest, want)
				}
			}
			if err := m.Apply(db); err != nil {
				t.Fatalf("repeat apply: %v", err)
			}
		})
	}
}
