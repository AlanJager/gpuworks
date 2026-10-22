package store

import (
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"
)

type scopeLabelRow struct {
	ID         uint
	Scope      string
	LabelKey   *string
	LabelValue *string
	Nodes      string
}

func scopeLabelRows(t *testing.T, db *gorm.DB) map[uint]scopeLabelRow {
	t.Helper()
	var rows []scopeLabelRow
	if err := db.Raw("SELECT id, scope, label_key, label_value, nodes FROM network_test_runs ORDER BY id").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]scopeLabelRow{}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out
}

func text(s string) *string { return &s }

// Each shape scope_detail took: a label is split on its first "=", and
// everything else leaves both label columns empty (NULL: the column was added
// after the row was written).
func TestNetworkTestRunScopeLabelMigration(t *testing.T) {
	m := migrationByVersion(t, 114)
	db := openStoreSchemaAt112(t, "network-test-scope-label")
	before := tableRows(t, db, "network_test_runs")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	columns := columnsOf(t, db, "network_test_runs")
	if columns["scope_detail"] || !columns["label_key"] || !columns["label_value"] {
		t.Fatalf("network_test_runs columns = %v", columns)
	}
	want := map[uint]scopeLabelRow{
		1: {ID: 1, Scope: "label", LabelKey: text("rack"), LabelValue: text("r2"), Nodes: `["c","d"]`},
		2: {ID: 2, Scope: "label", LabelKey: text("k"), LabelValue: text("a=b"), Nodes: `["c","d"]`},
		3: {ID: 3, Scope: "label", LabelKey: text("zone"), LabelValue: text(""), Nodes: `["c","d"]`},
		4: {ID: 4, Scope: "label", Nodes: `["c","d"]`},
		5: {ID: 5, Scope: "nodes", Nodes: `["u1","u2"]`},
		6: {ID: 6, Scope: "all", Nodes: `["a","b","c"]`},
	}
	if got := scopeLabelRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	// Every other column keeps its value.
	if got := withoutColumns(tableRows(t, db, "network_test_runs"), "label_key", "label_value"); !reflect.DeepEqual(got, withoutColumns(before, "scope_detail")) {
		t.Fatalf("rows = %v, want %v", got, withoutColumns(before, "scope_detail"))
	}
	// The entity reads the backfilled row back.
	var run NetworkTestRun
	if err := db.First(&run, 2).Error; err != nil || run.LabelKey != "k" || run.LabelValue != "a=b" {
		t.Fatalf("run = %+v err=%v", run, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 114).Error; err != nil || marker.Name != "network_test_run_scope_label" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying on the migrated schema: %v", err)
	}
	if got := scopeLabelRows(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("re-applying changed rows: %+v", got)
	}
}

// A database that has both shapes (the label columns were added beside
// scope_detail) fills only the empty label columns, then drops the old one.
func TestNetworkTestRunScopeLabelMigrationKeepsFilledLabelColumns(t *testing.T) {
	db := openStoreSchemaAt112(t, "network-test-scope-label-both")
	for _, statement := range []string{
		"ALTER TABLE network_test_runs ADD COLUMN `label_key` text",
		"ALTER TABLE network_test_runs ADD COLUMN `label_value` text",
		`UPDATE network_test_runs SET label_key = 'rack', label_value = 'r9' WHERE id = 1`,
		`INSERT INTO network_test_runs (id, network_id, status, scope, scope_detail, label_key, label_value, nodes) VALUES (7, 1, 'done', 'label', 'pool=old', 'pool', '', '["x","y"]')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 114)}); err != nil {
		t.Fatal(err)
	}
	if columnsOf(t, db, "network_test_runs")["scope_detail"] {
		t.Fatal("scope_detail survived")
	}
	rows := scopeLabelRows(t, db)
	if row := rows[1]; row.LabelKey == nil || *row.LabelKey != "rack" || row.LabelValue == nil || *row.LabelValue != "r9" {
		t.Fatalf("filled row 1 was overwritten: %+v", row)
	}
	if row := rows[7]; row.LabelKey == nil || *row.LabelKey != "pool" || row.LabelValue == nil || *row.LabelValue != "" {
		t.Fatalf("filled row 7 was overwritten: %+v", row)
	}
	if row := rows[2]; row.LabelKey == nil || *row.LabelKey != "k" || row.LabelValue == nil || *row.LabelValue != "a=b" {
		t.Fatalf("empty row 2 was not filled: %+v", row)
	}
}

func TestNetworkTestRunScopeLabelMigrationRollsBackWithItsMarker(t *testing.T) {
	m := migrationByVersion(t, 114)
	db := openStoreSchemaAt112(t, "network-test-scope-label-rollback")
	before := tableRows(t, db, "network_test_runs")
	want := errors.New("injected scope label failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	columns := columnsOf(t, db, "network_test_runs")
	if !columns["scope_detail"] || columns["label_key"] || columns["label_value"] {
		t.Fatalf("the failed migration left schema changes behind: %v", columns)
	}
	if got := tableRows(t, db, "network_test_runs"); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows = %v, want %v", got, before)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 114).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if row := scopeLabelRows(t, db)[1]; columnsOf(t, db, "network_test_runs")["scope_detail"] || row.LabelKey == nil || *row.LabelKey != "rack" {
		t.Fatalf("restart after rollback did not migrate: %+v", row)
	}
}
