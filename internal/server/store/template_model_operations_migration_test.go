package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func templateModelOperationsDB(t *testing.T, name string) *gorm.DB {
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
		// 1: a template-created row, deployed twice; the newer snapshot wins.
		// 2: a [] stays: this one-off backfill predates [] meaning "not declared"
		//    (a template deploy fills it now) and only ever rewrote empty columns.
		// 3: an empty row whose only deployment had no template.
		// 4: an empty row whose template declared no operations.
		// 5: an empty row whose snapshot is unreadable.
		// 6: a row with operations of its own.
		`INSERT INTO models(id, operations) VALUES (1, ''), (2, '[]'), (3, NULL), (4, ''), (5, ''), (6, '[{"type":"openai.chat"}]')`,
		`INSERT INTO deployments(id, model_id, template_snapshot) VALUES
			(10, 1, '{"api":{"health_endpoint":"/health","operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]}}'),
			(11, 1, '{"engine":"vllm","api":{"health_endpoint":"/health","operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings","inputs":[]}]}}'),
			(12, 2, '{"api":{"operations":[{"type":"openai.chat"}]}}'),
			(13, 3, ''),
			(14, 4, '{"api":{"health_endpoint":"/health","operations":[]}}'),
			(15, 5, '{not json'),
			(16, 6, '{"api":{"operations":[{"type":"openai.embeddings"}]}}')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func modelOperationsColumn(t *testing.T, db *gorm.DB) map[uint]string {
	t.Helper()
	var rows []struct {
		ID         uint
		Operations *string
	}
	if err := db.Table("models").Order("id").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]string{}
	for _, row := range rows {
		if row.Operations == nil {
			out[row.ID] = "<null>"
			continue
		}
		out[row.ID] = *row.Operations
	}
	return out
}

func TestTemplateModelOperationsMigrationBackfillsFromTheNewestSnapshotAndIsIdempotent(t *testing.T) {
	db := templateModelOperationsDB(t, "template-model-operations-migration")
	m := migrationByVersion(t, 129)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	want := map[uint]string{
		1: `[{"type":"openai.embeddings","endpoint":"/v1/embeddings","inputs":[]}]`,
		2: `[]`,
		3: "<null>",
		4: "",
		5: "",
		6: `[{"type":"openai.chat"}]`,
	}
	got := modelOperationsColumn(t, db)
	for id, operations := range want {
		if got[id] != operations {
			t.Fatalf("model %d operations=%q want %q (all=%v)", id, got[id], operations, got)
		}
	}
	// A deployment created after the backfill must not be re-applied over it.
	if err := db.Exec(`INSERT INTO deployments(id, model_id, template_snapshot) VALUES (17, 1, '{"api":{"operations":[{"type":"openai.chat"}]}}')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if again := modelOperationsColumn(t, db); again[1] != want[1] {
		t.Fatalf("second run changed model 1: %q", again[1])
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 129).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
	// Running the body again by hand is a no-op too: every condition it writes
	// under is "still empty".
	if err := backfillTemplateModelOperations(db); err != nil {
		t.Fatal(err)
	}
	if again := modelOperationsColumn(t, db); again[1] != want[1] {
		t.Fatalf("direct rerun changed model 1: %q", again[1])
	}
}

func TestTemplateModelOperationsMigrationRollsBackWithItsMarker(t *testing.T) {
	db := templateModelOperationsDB(t, "template-model-operations-rollback")
	injected := errors.New("injected failure after the backfill")
	failing := migrationByVersion(t, 129)
	apply := failing.Apply
	failing.Apply = func(tx *gorm.DB) error {
		if err := apply(tx); err != nil {
			return err
		}
		return injected
	}
	if err := applyMigration(db, failing); !errors.Is(err, injected) {
		t.Fatalf("applyMigration error=%v, want %v", err, injected)
	}
	if got := modelOperationsColumn(t, db); got[1] != "" {
		t.Fatalf("backfill survived the failed migration: %q", got[1])
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 129).First(&marker).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("marker after rollback=%+v err=%v", marker, err)
	}
	// The next start applies the real migration cleanly.
	if err := migrateDB(db, []migration{migrationByVersion(t, 129)}); err != nil {
		t.Fatalf("migration after rollback: %v", err)
	}
	if got := modelOperationsColumn(t, db); got[1] != `[{"type":"openai.embeddings","endpoint":"/v1/embeddings","inputs":[]}]` {
		t.Fatalf("model 1 after restart=%q", got[1])
	}
}

func TestTemplateModelOperationsMigrationSkipsADatabaseWithoutTheTables(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:template-model-operations-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillTemplateModelOperations(db); err != nil {
		t.Fatalf("empty database: %v", err)
	}
}
