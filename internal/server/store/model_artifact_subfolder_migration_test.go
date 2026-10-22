package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestModelArtifactSubfolderMigrationAddsIdentityAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model_artifact_subfolder_migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
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
	if err := db.Exec("INSERT INTO models(id, source, source_id, revision) VALUES(1, 'modelscope', 'MiniMax/MiniMax-H3', 'commit')").Error; err != nil {
		t.Fatal(err)
	}

	m := migrationByVersion(t, 71)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Model{}, "ArtifactSubfolder") {
		t.Fatal("models.artifact_subfolder was not added")
	}
	var subfolder *string
	if err := db.Raw("SELECT artifact_subfolder FROM models WHERE id = 1").Scan(&subfolder).Error; err != nil || subfolder == nil || *subfolder != "" {
		t.Fatalf("legacy subfolder=%v err=%v", subfolder, err)
	}
	if err := db.Exec("INSERT INTO models(id, source, source_id, revision, artifact_subfolder) VALUES(2, 'modelscope', 'minimax/minimax-h3', 'commit', 'FL2VA')").Error; err != nil {
		t.Fatalf("distinct subfolder identity was rejected: %v", err)
	}
	if err := db.Exec("INSERT INTO models(id, source, source_id, revision, artifact_subfolder) VALUES(3, 'MODELSCOPE', 'MINIMAX/MINIMAX-H3', 'commit', 'FL2VA')").Error; err == nil {
		t.Fatal("casefold duplicate subfolder identity was accepted")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestModelArtifactSubfolderMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model_artifact_subfolder_rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE models (id INTEGER PRIMARY KEY, source TEXT, source_id TEXT, revision TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected model artifact migration failure")
	failing := migration{Version: 71, Name: "model_artifact_subfolder_identity", Apply: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE models ADD COLUMN artifact_subfolder TEXT").Error; err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&Model{}, "ArtifactSubfolder") {
		t.Fatal("failed migration left artifact_subfolder behind")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 71).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 71)}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}
