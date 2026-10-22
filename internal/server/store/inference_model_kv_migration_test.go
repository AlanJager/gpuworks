package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// A database created before the catalog carried KV costs has no column for
// them, and the baseline AutoMigrate has already run by then, so the versioned
// migration is the only thing that adds it. Without it the projection drops
// every value the seed catalog ships and the deploy form reports no capacity
// for models the catalog knows about.
func TestInferenceModelKVBytesPerTokenMigrationIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:kv_bytes_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, model_id TEXT NOT NULL, source TEXT NOT NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	migration := migrationByVersion(t, 86)
	if err := applyMigration(db, migration); err != nil {
		t.Fatal(err)
	}
	if err := migration.Apply(db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !db.Migrator().HasColumn(&InferenceModel{}, "KVBytesPerToken") {
		t.Fatal("kv_bytes_per_token column was not added")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 86).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
}

// A database that never had the flat catalog table must not gain one here: the
// migration adds a column to an existing table, it does not create the table.
func TestInferenceModelKVBytesPerTokenMigrationSkipsWithoutTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:kv_bytes_no_table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrationByVersion(t, 86).Apply(db); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&InferenceModel{}) {
		t.Fatal("migration created the flat catalog table")
	}
}
