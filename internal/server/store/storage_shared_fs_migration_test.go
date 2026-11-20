package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// Node states from before the shared filesystem columns gain them and read as
// on a local disk; the migration is idempotent, rolls back with its marker,
// and applies again after a rollback.
func TestStorageNodeSharedFSMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&StorageNodeState{StorageID: 1, NodeID: 1, State: "ready", Path: "/opt/gpuworks/models", Writable: true}).Error; err != nil {
		t.Fatal(err)
	}
	cols := []string{"shared_fs_type", "shared_fs_source"}
	if err := dropColumns(s.db, "storage_node_states", cols...); err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "storage_node_shared_fs")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	has := func() map[string]bool {
		out := map[string]bool{}
		for _, c := range []string{"SharedFSType", "SharedFSSource"} {
			out[c] = s.db.Migrator().HasColumn(&StorageNodeState{}, c)
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
			t.Fatalf("column %s missing", c)
		}
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "storage_node_shared_fs" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	var row StorageNodeState
	if err := s.db.First(&row).Error; err != nil || row.SharedFSType != "" || row.SharedFSSource != "" || row.Path != "/opt/gpuworks/models" || !row.Writable {
		t.Fatalf("old row = %+v (%v)", row, err)
	}
	if err := dropColumns(s.db, "storage_node_states", cols...); err != nil {
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
