package store

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The peer-memory column arrives empty, a second application changes
// nothing, and a failed marker write leaves no column behind.
func TestPeerMemoryMigration(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_peer_memory")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "peer_memory") {
		t.Fatal("peer_memory is missing after the migration")
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failed marker leaves no column.
func TestPeerMemoryMigrationRollsBack(t *testing.T) {
	m := migrationByName(t, "node_peer_memory")
	failing := nodesOnlyDB(t, "")
	if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_peer_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(failing, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if failing.Migrator().HasColumn("nodes", "peer_memory") {
		t.Fatal("the column survived a failed migration")
	}
}

// Each status report replaces the node's peer-memory state; an agent that
// does not report it leaves the node unknown.
func TestUpdateNodeStatusRecordsPeerMemory(t *testing.T) {
	s := newTestStore(t)
	node, _, err := s.CreateNodePending("peer", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{proto.PeerMemoryMissing, proto.PeerMemoryLoaded, ""} {
		if err := s.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{PeerMemory: state}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PeerMemory != state {
			t.Fatalf("after reporting %q: %q", state, got.PeerMemory)
		}
	}
}
