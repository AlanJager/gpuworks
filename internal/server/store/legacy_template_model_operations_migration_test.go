package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// legacyChatPlaceholderFixture is the value migration 80 wrote, spelled the
// way the fixture stores it. The repair recognizes the placeholder
// structurally (IsDefaultChatOperations); this literal only lets a test
// assert that a particular row was left untouched.
const legacyChatPlaceholderFixture = `[{"type":"openai.chat"}]`

func legacyTemplateOperationsDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE models (id INTEGER PRIMARY KEY, operations TEXT)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, model_id INTEGER, template_snapshot TEXT)`,
		`INSERT INTO models(id, operations) VALUES
			(1, '[{"type":"openai.chat"}]'),
			(2, '[{"type":"openai.chat"}]'),
			(3, '[{"type":"openai.chat"}]'),
			(4, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(5, '[]'),
			(6, '[{"type":"openai.chat"}]'),
			(7, '[{"inputs":null,"type":"openai.chat"}]')`,
		`INSERT INTO deployments(id, model_id, template_snapshot) VALUES
			(10, 1, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(11, 2, '{"api":{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}}'),
			(12, 3, '{"api":{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}}'),
			(13, 3, '{"api":{"operations":[{"type":"openai.chat"}]}}'),
			(14, 4, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(15, 5, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(16, 6, '{"api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(17, 7, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestLegacyTemplateModelOperationsMigrationRepairsOnlyTheBackfill(t *testing.T) {
	db := legacyTemplateOperationsDB(t, "legacy_template_operations")
	application := migrationByVersion(t, 172)
	if err := migrateDB(db, []migration{application}); err != nil {
		t.Fatal(err)
	}
	got := modelOperationsColumn(t, db)
	want := map[uint]string{
		1: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`,
		2: `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`,
		3: `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`,
		4: `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		5: `[]`,
		6: `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		7: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`,
	}
	for id, operations := range want {
		if got[id] != operations {
			t.Fatalf("model %d operations=%q want %q (all=%v)", id, got[id], operations, got)
		}
	}
	if err := migrateDB(db, []migration{application}); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestLegacyTemplateModelOperationsMigrationRollsBackWithItsMarker(t *testing.T) {
	db := legacyTemplateOperationsDB(t, "legacy_template_operations_rollback")
	injected := errors.New("injected failure after legacy operations repair")
	application := migrationByVersion(t, 172)
	apply := application.Apply
	application.Apply = func(tx *gorm.DB) error {
		if err := apply(tx); err != nil {
			return err
		}
		return injected
	}
	if err := applyMigration(db, application); !errors.Is(err, injected) {
		t.Fatalf("applyMigration error=%v, want %v", err, injected)
	}
	if got := modelOperationsColumn(t, db); got[1] != legacyChatPlaceholderFixture {
		t.Fatalf("repair survived failed migration: %q", got[1])
	}
}

func TestLegacyTemplateModelOperationsMigrationSkipsSparseSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:legacy_template_operations_sparse?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := repairLegacyTemplateModelOperations(db); err != nil {
		t.Fatalf("sparse database: %v", err)
	}
}
