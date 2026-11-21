package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// A database that carries the node placement policy column loses it, a node
// that had the setting on keeps every other column and gets no card enabled
// for it, and the migration is idempotent; schema and marker roll back
// together, and a database that rolled back starts again and migrates.
func TestDropNodePlacementPolicyMigration(t *testing.T) {
	s := newTestStore(t)
	const column = "placement_share_with_foreign"
	if err := s.db.Exec("ALTER TABLE nodes ADD COLUMN " + column + " numeric NOT NULL DEFAULT false").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&Node{UUID: "shared", Name: "shared"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("UPDATE nodes SET "+column+" = 1 WHERE uuid = ?", "shared").Error; err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeByUUID("shared")
	if err != nil {
		t.Fatal(err)
	}
	held := GPU{NodeID: node.ID, Index: 0, Ident: "GPU-held", UUID: "GPU-held", ForeignProcesses: `[{"pid":41235,"name":"python3","memory_bytes":1073741824}]`}
	if err := s.db.Create(&held).Error; err != nil {
		t.Fatal(err)
	}
	indexesBefore, err := indexDefinitions(s.db, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "drop_node_placement_policy")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}

	boom := errors.New("after drop")
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
	if present, err := tableHasColumn(s.db, "nodes", column); err != nil || !present {
		t.Fatalf("the failed transaction dropped the column: present=%v err=%v", present, err)
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
	if present, err := tableHasColumn(s.db, "nodes", column); err != nil || present {
		t.Fatalf("nodes.%s still present: present=%v err=%v", column, present, err)
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "drop_node_placement_policy" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	indexesAfter, err := indexDefinitions(s.db, "nodes")
	if err != nil || len(indexesAfter) != len(indexesBefore) {
		t.Fatalf("nodes indexes = %d (%v), want the %d it had", len(indexesAfter), err, len(indexesBefore))
	}
	if kept, err := s.NodeByUUID("shared"); err != nil || kept.Name != "shared" {
		t.Fatalf("node after the drop = %+v %v", kept, err)
	}
	// The node's setting is not carried over to its cards.
	var card GPU
	if err := s.db.First(&card, held.ID).Error; err != nil || card.SchedulingAllowedAt != nil || card.CanTakeWork() {
		t.Fatalf("held card after the drop = allowed %v takes work %v (%v), want left to the platform", card.SchedulingAllowedAt, card.CanTakeWork(), err)
	}
}

// The retired migration that once added the column leaves a database that
// never ran it without the column, so the drop has nothing to do there.
func TestRetiredNodePlacementPolicyMigrationAddsNothing(t *testing.T) {
	s := newTestStore(t)
	retired, drop := migrationByName(t, "node_placement_policy"), migrationByName(t, "drop_node_placement_policy")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version IN ?", []int{retired.Version, drop.Version}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{retired, drop}); err != nil {
		t.Fatal(err)
	}
	if present, err := tableHasColumn(s.db, "nodes", "placement_share_with_foreign"); err != nil || present {
		t.Fatalf("column present=%v err=%v, want absent", present, err)
	}
	var markers int64
	if err := s.db.Model(&SchemaMigration{}).Where("version IN ?", []int{retired.Version, drop.Version}).Count(&markers).Error; err != nil || markers != 2 {
		t.Fatalf("markers = %d %v, want both", markers, err)
	}
}
