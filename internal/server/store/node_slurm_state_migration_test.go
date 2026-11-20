package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// A database whose nodes predate Slurm state gains the three columns; the old
// rows read as not on Slurm, and the migration is idempotent, rolls back with
// its marker, and applies again after a rollback.
func TestNodeSlurmStateMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Node{UUID: "legacy", Hostname: "old-host"}).Error; err != nil {
		t.Fatal(err)
	}
	cols := []string{"slurm_node", "slurm_unavailable", "slurm_reason"}
	if err := dropColumns(s.db, "nodes", cols...); err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_slurm_state")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	has := func() map[string]bool {
		out := map[string]bool{}
		for _, c := range []string{"SlurmNode", "SlurmUnavailable", "SlurmReason"} {
			out[c] = s.db.Migrator().HasColumn(&Node{}, c)
		}
		return out
	}

	boom := errors.New("after columns")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("rollback = %v", err)
	}
	for c, ok := range has() {
		if ok {
			t.Fatalf("the failed transaction kept column %s", c)
		}
	}
	var count int64
	if err := s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("the failed transaction kept the marker: %d %v", count, err)
	}

	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	for c, ok := range has() {
		if !ok {
			t.Fatalf("nodes column %s missing", c)
		}
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "node_slurm_state" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	node, err := s.NodeByUUID("legacy")
	if err != nil || node.SlurmNode != "" || node.SlurmUnavailable != "" || node.SlurmReason != "" || node.Hostname != "old-host" {
		t.Fatalf("legacy node = %+v (%v)", node, err)
	}

	// After a rollback the next start applies it.
	if err := dropColumns(s.db, "nodes", cols...); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	for c, ok := range has() {
		if !ok {
			t.Fatalf("after rollback, column %s missing", c)
		}
	}
}
