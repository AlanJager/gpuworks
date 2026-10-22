package store

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The column for why a container runtime cannot use this node's cards
// arrives, and the nullable bool that briefly held the same fact goes with
// it. Both halves are in one migration because they are one change: a
// database carrying the old column has no field left to read it.
func TestRuntimeGPUBlockedMigrationAddsItsColumnAndRemovesTheOldOne(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_gpu_ready numeric")
	m := migrationByName(t, "node_runtime_gpu_blocked")

	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "runtime_gpu_blocked") {
		t.Fatal("the reason column is missing after the migration")
	}
	if db.Migrator().HasColumn("nodes", "runtime_gpu_ready") {
		t.Fatal("runtime_gpu_ready outlived the fact it answered")
	}
	// Applying the schema change again changes nothing: a database that
	// never had the old column takes the same path.
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// A database that never ran the short-lived migration has nothing to drop.
func TestRuntimeGPUBlockedMigrationRunsWithoutTheOldColumn(t *testing.T) {
	db := nodesOnlyDB(t, "")
	if err := migrateDB(db, []migration{migrationByName(t, "node_runtime_gpu_blocked")}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "runtime_gpu_blocked") {
		t.Fatal("the reason column is missing after the migration")
	}
}

// Schema and marker are one transaction: a failure leaves neither.
func TestRuntimeGPUBlockedMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_gpu_ready numeric")
	m := migrationByName(t, "node_runtime_gpu_blocked")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_runtime_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "runtime_gpu_blocked") {
		t.Fatal("the new column survived a failed migration")
	}
	if !db.Migrator().HasColumn("nodes", "runtime_gpu_ready") {
		t.Fatal("the dropped column was not restored by the rollback")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}

	if err := db.Exec(`DROP TRIGGER fail_runtime_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "runtime_gpu_blocked") || db.Migrator().HasColumn("nodes", "runtime_gpu_ready") {
		t.Fatal("the retry did not leave the schema the migration describes")
	}
}

// nodesOnlyDB is a database with just the nodes table these migrations
// touch, optionally carrying extra columns an earlier schema left.
func nodesOnlyDB(t *testing.T, extraColumns string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	columns := "id INTEGER PRIMARY KEY, uuid TEXT"
	if extraColumns != "" {
		columns += ", " + extraColumns
	}
	if err := db.Exec("CREATE TABLE nodes (" + columns + ")").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	return db
}
