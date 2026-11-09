package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func modelSourceIDIndexMigration(t *testing.T) migration {
	t.Helper()
	for _, m := range migrations {
		if m.Name == "model_source_id_index" {
			return m
		}
	}
	t.Fatal("model_source_id_index migration is missing")
	return migration{}
}

func modelIndexNames(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'models'").Scan(&names).Error; err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	return found
}

func TestModelSourceIDIndexMigrationIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model-source-id-index?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Model{}); err != nil {
		t.Fatal(err)
	}
	m := modelSourceIDIndexMigration(t)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !modelIndexNames(t, db)["idx_model_source_id_lower"] {
		t.Fatalf("index was not created; have %v", modelIndexNames(t, db))
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", m.Version).First(&marker).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v err = %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count = %d err = %v", markers, err)
	}
}

func TestModelSourceIDIndexMigrationSkipsAMissingTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model-source-id-index-no-table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{modelSourceIDIndexMigration(t)}); err != nil {
		t.Fatalf("migration without a models table: %v", err)
	}
	if db.Migrator().HasTable(&Model{}) {
		t.Fatal("the migration created the table it was supposed to skip")
	}
}

// A failure after the index is created leaves neither the index nor the
// marker, so a retry starts from the same place.
func TestModelSourceIDIndexMigrationRollsBackOnFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model-source-id-index-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Model{}); err != nil {
		t.Fatal(err)
	}
	real := modelSourceIDIndexMigration(t)
	injected := errors.New("injected failure")
	err = applyMigration(db, migration{Version: real.Version, Name: real.Name, Apply: func(tx *gorm.DB) error {
		if err := real.Apply(tx); err != nil {
			return err
		}
		return injected
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("applyMigration error = %v, want the injected failure", err)
	}
	if modelIndexNames(t, db)["idx_model_source_id_lower"] {
		t.Fatal("the index survived a failed migration")
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", real.Version).First(&marker).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("marker after rollback = %+v err = %v", marker, err)
	}
	if err := migrateDB(db, []migration{real}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !modelIndexNames(t, db)["idx_model_source_id_lower"] {
		t.Fatal("retry did not create the index")
	}
}

// The deploy evaluation's lookup reads through the repository-id index, not
// every model row.
func TestModelArtifactsBySourceIDsUsesTheIndex(t *testing.T) {
	s := newTestStore(t)
	stmt := modelArtifactsBySourceIDsQuery(s.db, []string{"acme/a", "acme/b"}).
		Session(&gorm.Session{DryRun: true}).Find(&[]Model{}).Statement
	var plan []struct{ Detail string }
	if err := s.db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, row := range plan {
		details = append(details, row.Detail)
	}
	joined := strings.Join(details, "; ")
	if !strings.Contains(joined, "idx_model_source_id_lower") {
		t.Fatalf("plan = %s, want idx_model_source_id_lower", joined)
	}
}

func TestModelArtifactsBySourceIDsMatchesCaseInsensitivelyAndReadsIdentityOnly(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []*Model{
		{Source: "huggingface", SourceID: "Acme/Model", Revision: "main", Meta: `{"big":"config"}`, README: "readme"},
		{Source: "modelscope", SourceID: "acme/model", Revision: "v1", ArtifactSubfolder: "fp8"},
		{Source: "local", SourceID: "Other", Revision: "local"},
	} {
		if _, err := s.UpsertModel(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ModelArtifactsBySourceIDs(" ACME/model ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Source != "modelscope" || got[1].Source != "huggingface" {
		t.Fatalf("models = %+v, want both Acme/Model rows newest first", got)
	}
	if got[0].ArtifactSubfolder != "fp8" || got[1].Revision != "main" || got[1].ID == 0 {
		t.Fatalf("identity columns missing: %+v", got)
	}
	if got[1].Meta != "" || got[1].README != "" {
		t.Fatalf("lookup read the text columns: %+v", got[1])
	}
	for _, ids := range [][]string{nil, {""}, {"  "}} {
		none, err := s.ModelArtifactsBySourceIDs(ids...)
		if err != nil || len(none) != 0 {
			t.Fatalf("ids %q returned %+v err=%v, want none", ids, none, err)
		}
	}
}
