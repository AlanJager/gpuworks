package store

import (
	"fmt"
	"testing"
)

// Where an SSH install put the agent arrives as one column, empty for every
// existing node (recovery reads that as the default directory). Applying the
// change again changes nothing.
func TestNodeInstallDirMigrationAddsItsColumn(t *testing.T) {
	db := nodesOnlyDB(t, "server_dial_listen text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, server_dial_listen) VALUES (1, 'n-1', ':19090')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_install_dir")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "install_dir") {
		t.Fatal("install_dir is missing after the migration")
	}
	var row struct{ InstallDir, ServerDialListen string }
	if err := db.Raw(`SELECT COALESCE(install_dir, '') AS install_dir, server_dial_listen FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.InstallDir != "" || row.ServerDialListen != ":19090" {
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
func TestNodeInstallDirMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_install_dir")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_install_dir_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "install_dir") {
		t.Fatal("the column survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_install_dir_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "install_dir") {
		t.Fatal("the retry did not add the column")
	}
}
