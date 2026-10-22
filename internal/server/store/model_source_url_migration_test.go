package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// A model registered from an HTTP directory index has no way to reconstruct
// that directory from any other column, so the column has to arrive on every
// existing database, exactly once, without disturbing the rows already there.
func TestModelSourceURLMigrationAddsTheColumnAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model_source_url_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE models (
		id INTEGER PRIMARY KEY,
		source TEXT,
		source_id TEXT,
		revision TEXT
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO models(id, source, source_id, revision) VALUES(1, 'huggingface', 'Qwen/Qwen3-0.6B', 'main')").Error; err != nil {
		t.Fatal(err)
	}

	m := migrationByName(t, "model_source_url")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Model{}, "SourceURL") {
		t.Fatal("models.source_url was not added")
	}
	// The version marker advanced, which is what keeps the migration from
	// running again on the next start.
	var marker SchemaMigration
	if err := db.Where("version = ?", m.Version).First(&marker).Error; err != nil {
		t.Fatalf("version marker missing: %v", err)
	}
	if marker.Name != "model_source_url" || marker.AppliedAt.IsZero() {
		t.Fatalf("version marker = %+v", marker)
	}

	// An existing row keeps its identity and reads back with no directory
	// URL, which is what every non-url model must look like.
	var legacy Model
	if err := db.First(&legacy, 1).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.SourceURL != "" || legacy.Source != "huggingface" || legacy.SourceID != "Qwen/Qwen3-0.6B" || legacy.Revision != "main" {
		t.Fatalf("legacy row = %+v", legacy)
	}

	// The column is wide enough for a real directory URL and round-trips.
	directory := "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"
	if err := db.Exec("INSERT INTO models(id, source, source_id, revision, source_url) VALUES(2, 'url', 'Qwen/Qwen3-0.6B', '', ?)", directory).Error; err != nil {
		t.Fatalf("url model was rejected: %v", err)
	}
	var stored Model
	if err := db.First(&stored, 2).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SourceURL != directory || stored.Source != "url" || stored.Revision != "" {
		t.Fatalf("stored url model = %+v", stored)
	}

	// Re-running is a no-op rather than a duplicate-column failure.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying the migration body failed: %v", err)
	}
	var stillThere Model
	if err := db.First(&stillThere, 2).Error; err != nil || stillThere.SourceURL != directory {
		t.Fatalf("re-run lost data: %+v err=%v", stillThere, err)
	}
}

// Schema change and version marker share one transaction, so a failure part
// way through must leave neither behind: a marker without the column would
// make the next start skip a migration that never happened.
func TestModelSourceURLMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model_source_url_rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE models (id INTEGER PRIMARY KEY, source TEXT, source_id TEXT, revision TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected model source url migration failure")
	base := migrationByName(t, "model_source_url")
	failing := migration{Version: base.Version, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&Model{}, "SourceURL") {
		t.Fatal("failed migration left models.source_url behind")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", base.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
	// The next start must be able to apply it cleanly.
	if err := migrateDB(db, []migration{base}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if !db.Migrator().HasColumn(&Model{}, "SourceURL") {
		t.Fatal("retry did not add models.source_url")
	}
}
