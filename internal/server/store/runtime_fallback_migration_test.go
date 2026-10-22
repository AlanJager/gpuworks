package store

import (
	"fmt"
	"testing"
)

// The runtime a node passed over and why arrive as two columns, empty for
// every existing node until its next report. Applying the change again
// changes nothing.
func TestRuntimeFallbackMigrationAddsItsColumns(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_gpu_blocked text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, runtime_gpu_blocked) VALUES (1, 'n-1', 'gpu_plugin_missing')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_runtime_fallback")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"runtime_fallback_from", "runtime_fallback_reason"} {
		if !db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("%s is missing after the migration", column)
		}
	}
	var row struct{ RuntimeFallbackFrom, RuntimeFallbackReason, RuntimeGPUBlocked string }
	if err := db.Raw(`SELECT COALESCE(runtime_fallback_from, '') AS runtime_fallback_from, COALESCE(runtime_fallback_reason, '') AS runtime_fallback_reason, runtime_gpu_blocked FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RuntimeFallbackFrom != "" || row.RuntimeFallbackReason != "" || row.RuntimeGPUBlocked != "gpu_plugin_missing" {
		t.Fatalf("existing node after the migration: %+v", row)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failure leaves neither, and the
// retry completes.
func TestRuntimeFallbackMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_runtime_fallback")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_fallback_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "runtime_fallback_from") || db.Migrator().HasColumn("nodes", "runtime_fallback_reason") {
		t.Fatal("a column survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_fallback_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "runtime_fallback_from") || !db.Migrator().HasColumn("nodes", "runtime_fallback_reason") {
		t.Fatal("the retry did not add the columns")
	}
}
