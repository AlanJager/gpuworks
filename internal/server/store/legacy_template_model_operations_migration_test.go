package store

import (
	"encoding/json"
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
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER, source TEXT, name TEXT, api_contract TEXT)`,
		`INSERT INTO models(id, operations) VALUES
			(1, '[{"type":"openai.chat"}]'),
			(2, '[{"type":"openai.chat"}]'),
			(3, '[{"type":"openai.chat"}]'),
			(4, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(5, '[]'),
			(6, '[{"type":"openai.chat"}]'),
			(7, '[{"inputs":null,"type":"openai.chat"}]'),
			(8, '[{"type":"openai.chat"}]'),
			(9, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(10, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(11, '[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]'),
			(12, '[{"type":"openai.chat"}]'),
			(13, '[{"type":"openai.chat"}]'),
			(14, '[{"type":"openai.chat"}]'),
			(15, '[{"type":"openai.chat"}]')`,
		`INSERT INTO deployments(id, model_id, template_snapshot) VALUES
			(10, 1, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(11, 2, '{"api":{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}}'),
			(12, 3, '{"api":{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}}'),
			(13, 3, '{"api":{"operations":[{"type":"openai.chat"}]}}'),
			(14, 4, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(15, 5, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(16, 6, '{"api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(17, 7, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(18, 6, '{"api":{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}}'),
			(19, 6, '{"api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(20, 8, '{"template_id":20,"spec_name":"embed","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(21, 9, '{"template_id":21,"spec_source":"user","spec_name":"rank","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(22, 10, '{"template_id":22,"spec_name":"chat","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(23, 11, '{"template_id":23,"spec_name":"unknown","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(24, 12, '{"template_id":21,"spec_source":"user","spec_name":"rank","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(25, 13, '{"template_id":22,"spec_name":"chat","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(26, 14, '{"template_id":23,"spec_name":"unknown","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}'),
			(27, 15, '{"template_id":24,"spec_name":"null-contract","api":{"operations":[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}}')`,
		`INSERT INTO inference_specs(id, model_id, source, name, api_contract) VALUES
			(1, 20, 'upstream', 'embed', '{"operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}'),
			(2, 21, 'user', 'rank', '{"operations":[{"type":"openai.rerank","endpoint":"/v1/rerank"}]}'),
			(3, 22, 'upstream', 'chat', '{"operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]}'),
			(4, 24, 'upstream', 'null-contract', NULL)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestLegacyTemplateNonChatOperationsUsesCatalogIdentity(t *testing.T) {
	db := legacyTemplateOperationsDB(t, "legacy_template_catalog_identity")
	for _, column := range []string{"model_id", "source", "name", "api_contract"} {
		if !db.Migrator().HasColumn("inference_specs", column) {
			t.Fatalf("inference_specs.%s is not visible", column)
		}
	}
	var raw string
	if err := db.Raw(`SELECT api_contract FROM inference_specs WHERE model_id = 20 AND source = 'upstream' AND name = 'embed'`).Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Operations json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal([]byte(raw), &contract); err != nil {
		t.Fatalf("parse catalog contract: %v", err)
	}
	operations := string(contract.Operations)
	if !legacyOperationsPresent(operations) || legacyChatOnlyOperations(operations) {
		t.Fatalf("unexpected catalog operations: %q", operations)
	}
	declared, ok, err := legacyTemplateNonChatOperations(db, templateSnapshotAPI{TemplateID: 20, SpecName: "embed"})
	if err != nil || !ok || declared != `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]` {
		t.Fatalf("catalog operations = %q, ok=%v, err=%v", declared, ok, err)
	}
	if !legacyNormalizedChatOperations(`[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`) {
		t.Fatal("legacy writer's normalized placeholder was not recognized")
	}
}

func TestLegacyTemplateModelOperationsMigrationRepairsPlaceholdersWithoutInventingContracts(t *testing.T) {
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
		6: legacyChatPlaceholderFixture,
		7: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`,
		8: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`,
		// Model 9 may be an explicit chat edit; a mutable catalog must not
		// overwrite that choice.
		9:  `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		10: `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		11: `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		12: `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`,
		13: legacyChatPlaceholderFixture,
		14: legacyChatPlaceholderFixture,
		15: legacyChatPlaceholderFixture,
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
	got := modelOperationsColumn(t, db)
	for id, want := range map[uint]string{
		1: legacyChatPlaceholderFixture,
		8: legacyChatPlaceholderFixture,
		9: `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
	} {
		if got[id] != want {
			t.Fatalf("model %d repair survived failed migration: %q, want %q", id, got[id], want)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 172).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("migration marker survived rollback: count=%d err=%v", markers, err)
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
