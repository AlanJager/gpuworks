package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestBatchTablesMigration(t *testing.T) {
	m := migrationByVersion(t, 185)
	if m.Name != "batch_tables" {
		t.Fatalf("migration 185 is %q", m.Name)
	}
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		// A database from before batches: a schema marker table and an
		// unrelated table the migration must leave alone.
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`CREATE TABLE api_keys (id INTEGER PRIMARY KEY, name TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		return db
	}

	db := open("batch-tables")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []any{&BatchFile{}, &Batch{}, &BatchMetadata{}} {
		if !db.Migrator().HasTable(e) {
			t.Fatalf("%T has no table", e)
		}
	}
	for _, column := range []string{"UserID", "InputFileID", "Status", "CheckpointOffset", "ExpiresAt", "CancelledAt"} {
		if !db.Migrator().HasColumn(&Batch{}, column) {
			t.Errorf("batches.%s missing", column)
		}
	}
	for _, index := range []string{"idx_batches_user_created", "InputFileID", "Status"} {
		if !db.Migrator().HasIndex(&Batch{}, index) {
			t.Errorf("batches index %s missing", index)
		}
	}
	for _, index := range []string{"idx_batch_files_user_created", "idx_batch_files_created"} {
		if !db.Migrator().HasIndex(&BatchFile{}, index) {
			t.Errorf("batch_files index %s missing", index)
		}
	}
	if !db.Migrator().HasIndex(&Batch{}, "idx_batches_created") {
		t.Error("batches index idx_batches_created missing")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 185).Error; err != nil || marker.Name != "batch_tables" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}

	// A rerun with the marker does nothing; without it, the migration is
	// idempotent over the tables it already made (and keeps their rows).
	if err := db.Create(&Batch{ID: "batch_keep", Endpoint: "/v1/embeddings", InputFileID: "file-1", CompletionWindow: "24h", Status: BatchValidating}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 185").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
	var kept int64
	if err := db.Model(&Batch{}).Where("id = ?", "batch_keep").Count(&kept).Error; err != nil || kept != 1 {
		t.Fatalf("row after rerun: %d err=%v", kept, err)
	}

	// A failure after the schema change leaves neither the tables nor the
	// marker, and the next start applies the migration cleanly.
	rollback := open("batch-tables-rollback")
	want := errors.New("injected batch_tables failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, e := range []any{&BatchFile{}, &Batch{}, &BatchMetadata{}} {
		if rollback.Migrator().HasTable(e) {
			t.Fatalf("%T table survived the failed migration", e)
		}
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if !rollback.Migrator().HasTable(&Batch{}) {
		t.Fatal("tables missing after the restart")
	}
}
