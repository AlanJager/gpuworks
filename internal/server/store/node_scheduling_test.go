package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestNodeSchedulingMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO nodes (id, uuid) VALUES (1, 'old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 121)

	db := open("node-scheduling")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Node{}, "Scheduling") {
		t.Fatal("scheduling was not added")
	}
	// An existing node stays schedulable.
	var cordoned int64
	if err := db.Raw("SELECT COUNT(*) FROM nodes WHERE scheduling <> ''").Scan(&cordoned).Error; err != nil || cordoned != 0 {
		t.Fatalf("existing nodes cordoned = %d err=%v", cordoned, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 121).Error; err != nil || marker.Name != "node_scheduling" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 121").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("node-scheduling-rollback")
	want := errors.New("injected scheduling failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Node{}, "Scheduling") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 121).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Node{}, "Scheduling") {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}

// Cordoning and draining are one write announced as a node change other than
// a routine status report, so every deployment is reconciled at once.
func TestSetNodeScheduling(t *testing.T) {
	st, err := Open("file:store_node_scheduling?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("cordon-node", HashToken("cordon"), "cordon"); err != nil {
		t.Fatal(err)
	}
	if node, _ := st.NodeByUUID("cordon-node"); node.Scheduling != NodeSchedulable || !node.Scheduling.TakesReplicas() {
		t.Fatalf("a new node is %q", node.Scheduling)
	}
	sub := st.Bus.Subscribe(8, "node")
	defer sub.Close()
	for _, want := range []NodeScheduling{NodeCordoned, NodeDraining, NodeSchedulable} {
		node, err := st.SetNodeScheduling("cordon-node", want)
		if err != nil || node.Scheduling != want {
			t.Fatalf("set %q = %+v err=%v", want, node, err)
		}
		if ev := <-sub.C(); ev.Key != "cordon-node" || ev.Reason != NodeChangeScheduling {
			t.Fatalf("event = %+v", ev)
		}
		if again, _ := st.NodeByUUID("cordon-node"); again.Scheduling != want || again.Scheduling.TakesReplicas() != (want == NodeSchedulable) {
			t.Fatalf("stored %q, want %q", again.Scheduling, want)
		}
	}
	if _, err := st.SetNodeScheduling("cordon-node", "paused"); err == nil {
		t.Fatal("an unknown scheduling state was stored")
	}
	if _, err := st.SetNodeScheduling("missing", NodeCordoned); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
}
