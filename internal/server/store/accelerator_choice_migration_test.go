package store

import (
	"fmt"
	"testing"
)

// The accelerator choice and the vendors on the bus arrive as two columns, empty for
// every existing node until its next report. Applying the change again
// changes nothing.
func TestAcceleratorChoiceMigrationAddsItsColumns(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_gpu_blocked text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, runtime_gpu_blocked) VALUES (1, 'n-1', 'gpu_plugin_missing')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_accelerator_choice")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"accelerator_preference", "accelerator_vendors"} {
		if !db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("%s is missing after the migration", column)
		}
	}
	var row struct{ AcceleratorPreference, AcceleratorVendors, RuntimeGPUBlocked string }
	if err := db.Raw(`SELECT COALESCE(accelerator_preference, '') AS accelerator_preference, COALESCE(accelerator_vendors, '') AS accelerator_vendors, runtime_gpu_blocked FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AcceleratorPreference != "" || row.AcceleratorVendors != "" || row.RuntimeGPUBlocked != "gpu_plugin_missing" {
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
func TestAcceleratorChoiceMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_accelerator_choice")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_accelerator_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "accelerator_preference") || db.Migrator().HasColumn("nodes", "accelerator_vendors") {
		t.Fatal("a column survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_accelerator_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "accelerator_preference") || !db.Migrator().HasColumn("nodes", "accelerator_vendors") {
		t.Fatal("the retry did not add the columns")
	}
}
