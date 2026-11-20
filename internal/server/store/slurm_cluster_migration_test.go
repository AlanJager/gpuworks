package store

import (
	"fmt"
	"testing"
)

// A database whose nodes predate Slurm clusters gains the two tables, the
// node column and the partial unique index; its nodes read as joined from no
// entry, and the migration is idempotent.
func TestSlurmClustersMigrationAddsTablesColumnAndIndex(t *testing.T) {
	db := nodesOnlyDB(t, "install_dir text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid) VALUES (1, 'n-1'), (2, 'n-2')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "slurm_clusters")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"slurm_clusters", "slurm_gpu_nodes"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("table %s missing after the migration", table)
		}
	}
	if !db.Migrator().HasColumn("nodes", "slurm_cluster_id") || !db.Migrator().HasColumn("nodes", "slurm_node") {
		t.Fatal("nodes columns missing after the migration")
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil || marker.Name != "slurm_clusters" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	var row struct{ SlurmClusterID int }
	if err := db.Raw(`SELECT COALESCE(slurm_cluster_id,0) AS slurm_cluster_id FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil || row.SlurmClusterID != 0 {
		t.Fatalf("existing node after the migration: %+v, %v", row, err)
	}
	// Nodes of no entry may share a Slurm name; one entry's nodes may not.
	if err := db.Exec(`UPDATE nodes SET slurm_node = 'gpu01'`).Error; err != nil {
		t.Fatalf("hand-installed nodes with one name must stay legal: %v", err)
	}
	if err := db.Exec(`UPDATE nodes SET slurm_cluster_id = 5 WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE nodes SET slurm_cluster_id = 5 WHERE id = 2`).Error; err == nil {
		t.Fatal("two nodes of one entry took one Slurm node name")
	}
	if err := db.Exec(`UPDATE nodes SET slurm_cluster_id = 6 WHERE id = 2`).Error; err != nil {
		t.Fatalf("the same name in another entry must be legal: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun through the runner: %v", err)
	}
}

// A migration that fails after its schema work leaves neither the tables, the
// column nor the marker, and runs again on the next start.
func TestSlurmClustersMigrationRollsBackWithItsMarkerAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "slurm_clusters")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_slurm_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasTable("slurm_clusters") || db.Migrator().HasTable("slurm_gpu_nodes") || db.Migrator().HasColumn("nodes", "slurm_cluster_id") {
		t.Fatal("schema survived a failed migration")
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = 'idx_node_slurm'`).Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("the index survived a failed migration: %d %v", n, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_slurm_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !db.Migrator().HasTable("slurm_clusters") || !db.Migrator().HasColumn("nodes", "slurm_cluster_id") {
		t.Fatal("retry left the schema incomplete")
	}
}

// A fresh store already has the index, so enrollment's find-or-create is held
// unique by the database as well as by the transaction.
func TestFreshStoreHasTheSlurmNodeIndex(t *testing.T) {
	s := newTestStore(t)
	var n int64
	if err := s.db.Raw(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_node_slurm' AND sql LIKE '%UNIQUE%'`).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("idx_node_slurm = %d, %v", n, err)
	}
}
