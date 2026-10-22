package tsdb

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// openRawSQLite opens a bare database/sql handle at path without running any
// migration, so tests can drive migrateSchema against partial migration lists
// and inspect the schema in between steps.
func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// columnNames lists a table's columns via PRAGMA table_info, for asserting a
// migration did or did not add a column.
func columnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return out
}

func schemaVersionMarkerExists(t *testing.T, db *sql.DB, version int) bool {
	t.Helper()
	applied, err := schemaVersionApplied(db, version)
	if err != nil {
		t.Fatalf("schemaVersionApplied(%d): %v", version, err)
	}
	return applied
}

func TestMigrateSchemaIsIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")

	// First open runs every migration.
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.WriteBatch([]Sample{sample("0", 1000, 1)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening the same file must not fail or re-run anything (no duplicate
	// column / index errors), and the data written before reopen must survive.
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query after reopen: %v", err)
	}
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("data lost across reopen: %+v", got)
	}
}

func TestMigrateSchemaAppliesInOrderAndBackfillsFromLabelsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db := openRawSQLite(t, path)

	// Bring the file to a pre-migration-2 state, as a real upgrade would find
	// it: baseline tables exist, instance/deployment columns do not.
	if err := migrateSchema(db, schemaMigrations[:1]); err != nil {
		t.Fatalf("baseline migration: %v", err)
	}
	cols := columnNames(t, db, "series")
	if cols["instance"] || cols["deployment"] {
		t.Fatalf("instance/deployment columns present before migration 2: %v", cols)
	}

	// Insert series rows directly, as WriteBatch would have before this
	// change: labels_json carries instance/deployment but there is no column
	// to hold them yet.
	inserts := []struct {
		key    string
		labels string
	}{
		{"k1", `{"node":"n1","instance":"inst-1","deployment":"7","engine":"vllm"}`},
		{"k2", `{"node":"n1","gpu":"0"}`}, // no instance/deployment at all
	}
	for _, ins := range inserts {
		if _, err := db.Exec(`INSERT INTO series(metric, labels_key, labels_json) VALUES(?, ?, ?)`,
			"gpuworks_infer_requests_running", ins.key, ins.labels); err != nil {
			t.Fatalf("seed insert %s: %v", ins.key, err)
		}
	}

	if err := migrateSchema(db, schemaMigrations); err != nil {
		t.Fatalf("full migration: %v", err)
	}

	cols = columnNames(t, db, "series")
	if !cols["instance"] || !cols["deployment"] {
		t.Fatalf("instance/deployment columns missing after migration 2: %v", cols)
	}

	var instance, deployment sql.NullString
	if err := db.QueryRow(`SELECT instance, deployment FROM series WHERE labels_key = ?`, "k1").
		Scan(&instance, &deployment); err != nil {
		t.Fatalf("select k1: %v", err)
	}
	if instance.String != "inst-1" || deployment.String != "7" {
		t.Fatalf("k1 backfilled as instance=%q deployment=%q, want inst-1/7", instance.String, deployment.String)
	}

	if err := db.QueryRow(`SELECT instance, deployment FROM series WHERE labels_key = ?`, "k2").
		Scan(&instance, &deployment); err != nil {
		t.Fatalf("select k2: %v", err)
	}
	if instance.Valid || deployment.Valid {
		t.Fatalf("k2 should have NULL instance/deployment, got instance=%v deployment=%v", instance, deployment)
	}

	if !schemaVersionMarkerExists(t, db, 1) || !schemaVersionMarkerExists(t, db, 2) {
		t.Fatal("expected schema_version markers for versions 1 and 2")
	}

	// Idempotent on re-run: applying the same list again must not error and
	// must not touch the marker table again.
	if err := migrateSchema(db, schemaMigrations); err != nil {
		t.Fatalf("re-run migration: %v", err)
	}
}

// TestMigrateSchemaRollsBackOnConflictingIndex forces migration 2 to fail
// partway through (after its ALTER TABLE/backfill statements, on its first
// CREATE INDEX) by pre-creating an incompatible object under the index's
// name, then asserts neither the schema change nor the version marker
// survived - and that fixing the conflict lets the migration succeed.
func TestMigrateSchemaRollsBackOnConflictingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db := openRawSQLite(t, path)
	if err := migrateSchema(db, schemaMigrations[:1]); err != nil {
		t.Fatalf("baseline migration: %v", err)
	}

	// A pre-existing object with the exact name migration 2 wants for its
	// first index, on a different table: CREATE INDEX must fail rather than
	// silently reuse or replace it.
	if _, err := db.Exec(`CREATE INDEX idx_series_metric_instance ON samples(ts_ms)`); err != nil {
		t.Fatalf("seed conflicting index: %v", err)
	}

	if err := migrateSchema(db, schemaMigrations); err == nil {
		t.Fatal("expected migration 2 to fail against the conflicting index name")
	}

	cols := columnNames(t, db, "series")
	if cols["instance"] || cols["deployment"] {
		t.Fatalf("column add survived rollback: %v", cols)
	}
	if schemaVersionMarkerExists(t, db, 2) {
		t.Fatal("version 2 marker survived rollback")
	}
	if !schemaVersionMarkerExists(t, db, 1) {
		t.Fatal("unrelated version 1 marker should be untouched")
	}

	// Remove the conflict and retry: the corrected environment must now let
	// the migration complete and record its marker.
	if _, err := db.Exec(`DROP INDEX idx_series_metric_instance`); err != nil {
		t.Fatalf("drop conflicting index: %v", err)
	}
	if err := migrateSchema(db, schemaMigrations); err != nil {
		t.Fatalf("retry after removing conflict: %v", err)
	}
	cols = columnNames(t, db, "series")
	if !cols["instance"] || !cols["deployment"] {
		t.Fatalf("columns missing after successful retry: %v", cols)
	}
	if !schemaVersionMarkerExists(t, db, 2) {
		t.Fatal("version 2 marker missing after successful retry")
	}
}

// TestApplySchemaMigrationRollsBackOnInjectedError mirrors the main store's
// migration_transaction_test.go fixture-based rollback test: a migration that
// makes a schema change and then deliberately fails must leave neither the
// change nor its marker behind, using a plain injected error rather than a
// real naming conflict.
func TestApplySchemaMigrationRollsBackOnInjectedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db := openRawSQLite(t, path)
	if _, err := db.Exec(`CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}

	injected := errors.New("injected migration failure")
	fixture := schemaMigration{Version: 9001, Name: "rollback-fixture", Apply: func(tx *sql.Tx) error {
		if _, err := tx.Exec(`CREATE TABLE rollback_fixture (id INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		return injected
	}}
	err := applySchemaMigration(db, fixture)
	if !errors.Is(err, injected) {
		t.Fatalf("applySchemaMigration error = %v, want %v", err, injected)
	}
	var exists int
	err = db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='rollback_fixture'`).Scan(&exists)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rollback_fixture table survived rollback, err=%v", err)
	}
	if schemaVersionMarkerExists(t, db, 9001) {
		t.Fatal("marker survived rollback")
	}

	// A corrected version of the same migration must now apply cleanly.
	fixed := schemaMigration{Version: 9001, Name: "rollback-fixture", Apply: func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE rollback_fixture (id INTEGER PRIMARY KEY)`)
		return err
	}}
	if err := migrateSchema(db, []schemaMigration{fixed}); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if !schemaVersionMarkerExists(t, db, 9001) {
		t.Fatal("marker missing after corrected retry")
	}
}

// TestMigrateSchemaSkipsAlreadyAppliedVersionIdempotently mirrors the main
// store's equivalent test: calling migrateSchema twice with a migration that
// counts its own invocations must apply it exactly once.
func TestMigrateSchemaSkipsAlreadyAppliedVersionIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db := openRawSQLite(t, path)
	applies := 0
	m := schemaMigration{Version: 9002, Name: "idempotent-fixture", Apply: func(tx *sql.Tx) error {
		applies++
		_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS idempotent_fixture (id INTEGER PRIMARY KEY)`)
		return err
	}}
	if err := migrateSchema(db, []schemaMigration{m}); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrateSchema(db, []schemaMigration{m}); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if applies != 1 {
		t.Fatalf("apply count = %d, want 1", applies)
	}
}
