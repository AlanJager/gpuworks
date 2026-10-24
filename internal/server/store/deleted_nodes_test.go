package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeletingANodeKeepsItsName(t *testing.T) {
	st := newTestStore(t)
	named, err := st.RegisterNode("uuid-named", HashToken("x"), "host-named")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RenameNode(named.UUID, "gpu-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("uuid-host", HashToken("y"), "host-only"); err != nil {
		t.Fatal(err)
	}
	live, err := st.RegisterNode("uuid-live", HashToken("z"), "host-live")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	for _, uuid := range []string{"uuid-named", "uuid-host"} {
		if err := st.DeleteNode(uuid); err != nil {
			t.Fatal(err)
		}
	}

	gone, err := st.DeletedNodes([]string{"uuid-named", "uuid-host", "uuid-live", "nope"})
	if err != nil || len(gone) != 2 || gone["uuid-named"].Name != "gpu-a" || gone["uuid-host"].Name != "host-only" ||
		gone["uuid-named"].DeletedAt.Before(before) {
		t.Fatalf("deleted = %+v %v", gone, err)
	}
	// A live node's own name wins; a deleted one's comes from what it left.
	names, err := st.NodeNamesByUUID([]string{live.UUID, "uuid-named", "nope"})
	if err != nil || len(names) != 2 || names[live.UUID] != (NodeName{Name: "host-live"}) || names["uuid-named"] != (NodeName{Name: "gpu-a", Deleted: true}) {
		t.Fatalf("names = %+v %v", names, err)
	}

	since, err := st.NodesDeletedSince(before)
	if err != nil || strings.Join(since, ",") != "uuid-host,uuid-named" {
		t.Fatalf("deleted since = %v %v", since, err)
	}
	if later, err := st.NodesDeletedSince(time.Now().Add(time.Hour)); err != nil || len(later) != 0 {
		t.Fatalf("deleted since later = %v %v", later, err)
	}
	if none, err := st.DeletedNodes(nil); err != nil || len(none) != 0 {
		t.Fatal("empty lookup")
	}
	if err := st.DeleteNode("uuid-named"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

// A node registered again under a deleted node's UUID is named as itself,
// and deleting it again keeps the newer name.
func TestDeletedNodeNameFollowsTheLastDeletion(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RegisterNode("uuid-again", HashToken("x"), "first"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode("uuid-again"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("uuid-again", HashToken("y"), "second"); err != nil {
		t.Fatal(err)
	}
	if names, err := st.NodeNamesByUUID([]string{"uuid-again"}); err != nil || names["uuid-again"] != (NodeName{Name: "second"}) {
		t.Fatalf("re-registered = %+v %v", names, err)
	}
	if err := st.DeleteNode("uuid-again"); err != nil {
		t.Fatal(err)
	}
	if gone, err := st.DeletedNodes([]string{"uuid-again"}); err != nil || len(gone) != 1 || gone["uuid-again"].Name != "second" {
		t.Fatalf("deleted again = %+v %v", gone, err)
	}
}

// The deleted name is written in the deleting transaction: a deletion that
// fails afterwards leaves no record behind, and one that fails to record
// deletes nothing.
func TestDeletedNodeRecordRollsBackWithTheDeletion(t *testing.T) {
	for _, table := range []string{"nodes", "deleted_nodes"} {
		t.Run(table, func(t *testing.T) {
			st := newTestStore(t)
			if _, err := st.RegisterNode("uuid-rb", HashToken("x"), "host-rb"); err != nil {
				t.Fatal(err)
			}
			trigger := "CREATE TRIGGER reject BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT, 'injected failure'); END"
			if table == "deleted_nodes" {
				trigger = "CREATE TRIGGER reject BEFORE INSERT ON deleted_nodes BEGIN SELECT RAISE(ABORT, 'injected failure'); END"
			}
			if err := st.DB().Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteNode("uuid-rb"); err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("delete error = %v", err)
			}
			if _, err := st.NodeByUUID("uuid-rb"); err != nil {
				t.Fatalf("node lost after rollback: %v", err)
			}
			if gone, err := st.DeletedNodes([]string{"uuid-rb"}); err != nil || len(gone) != 0 {
				t.Fatalf("record survived the rollback: %+v %v", gone, err)
			}
		})
	}
}

func TestDeletedNodesMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 171)
	db := open("deleted-nodes")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("deleted_nodes") {
		t.Fatal("deleted_nodes was not created")
	}
	for _, column := range []string{"uuid", "name", "deleted_at"} {
		if !db.Migrator().HasColumn(&DeletedNode{}, column) {
			t.Fatalf("column %s missing", column)
		}
	}
	if !db.Migrator().HasIndex(&DeletedNode{}, "idx_deleted_nodes_deleted_at") {
		t.Fatal("deleted_at index missing")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 171).Error; err != nil || marker.Name != "deleted_nodes" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 171").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("deleted-nodes-rollback")
	want := errors.New("injected deleted nodes failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasTable("deleted_nodes") {
		t.Fatal("deleted_nodes survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 171).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d %v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasTable("deleted_nodes") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
