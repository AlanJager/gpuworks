package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Migration 119 deletes whatever an operator wrote in the gpuworks.ai
// namespace: the key from a node's labels, in any letter case and at any
// subdomain depth, and the network attachment that selected on it. A
// deployment's selector is not touched, because dropping a key from it would
// widen where the deployment may run.
func TestFreeReservedLabelNamespaceMigration(t *testing.T) {
	const (
		reservedLabels = `{"gpuworks.ai/pool":"a","rack.GPUWORKS.AI/row":"7","pool":"b"}`
		// Upper case alone, so a case-sensitive pre-filter would skip it.
		upperOnly   = `{"GPUWORKS.AI/zone":"z","team":"infra"}`
		plainLabels = `{"pool":"b","team":"infra"}`
		// Near misses are someone else's namespace and stay.
		nearMisses   = `{"gpuworks-ai/pool":"x","notgpuworks.ai/y":"1","gpuworks.ai.example.com/z":"2","gpuworks.ai/k":"gone"}`
		brokenLabels = `not json gpuworks.ai/x`

		reservedSelector = `{"mode":"manual","node_uuid":"n-1","node_labels":{"gpuworks.ai/pool":"a","team":"infra"},"tp":2}`
	)
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			"CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, labels TEXT)",
			"CREATE TABLE deployments (id INTEGER PRIMARY KEY, placement TEXT)",
			"CREATE TABLE network_attachments (id INTEGER PRIMARY KEY, network_id INTEGER, node_id INTEGER, label_key TEXT, label_value TEXT)",
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		for id, labels := range map[int]any{
			1: reservedLabels,
			2: plainLabels,
			3: upperOnly,
			4: brokenLabels,
			5: nil,
			6: nearMisses,
		} {
			if err := db.Exec("INSERT INTO nodes (id, uuid, labels) VALUES (?, ?, ?)", id, id, labels).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Exec("INSERT INTO deployments (id, placement) VALUES (1, ?)", reservedSelector).Error; err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			id, node   int
			key, value string
		}{
			{1, 0, "gpuworks.ai/pool", "a"},
			{2, 0, "pool", "b"},
			{3, 0, "Rack.GPUWorks.AI/row", "7"},
			{4, 0, "gpuworks-ai/pool", "x"},
			// A node attachment has no label key and is never touched.
			{5, 2, "", ""},
		} {
			if err := db.Exec("INSERT INTO network_attachments (id, network_id, node_id, label_key, label_value) VALUES (?, 1, ?, ?, ?)",
				row.id, row.node, row.key, row.value).Error; err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	column := func(db *gorm.DB, table, name string, id int) string {
		t.Helper()
		var raw *string
		if err := db.Raw("SELECT "+name+" FROM "+table+" WHERE id = ?", id).Scan(&raw).Error; err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return "<null>"
		}
		return *raw
	}
	sameJSON := func(t *testing.T, got, want string) bool {
		t.Helper()
		var g, w any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatalf("want %s: %v", want, err)
		}
		return json.Unmarshal([]byte(got), &g) == nil && reflect.DeepEqual(g, w)
	}
	attachments := func(t *testing.T, db *gorm.DB) []int {
		t.Helper()
		var ids []int
		if err := db.Raw("SELECT id FROM network_attachments ORDER BY id").Scan(&ids).Error; err != nil {
			t.Fatal(err)
		}
		return ids
	}
	check := func(t *testing.T, db *gorm.DB) {
		t.Helper()
		for id, want := range map[int]string{
			1: `{"pool":"b"}`,
			3: `{"team":"infra"}`,
			6: `{"gpuworks-ai/pool":"x","notgpuworks.ai/y":"1","gpuworks.ai.example.com/z":"2"}`,
		} {
			if got := column(db, "nodes", "labels", id); !sameJSON(t, got, want) {
				t.Errorf("node %d labels = %s\nwant %s", id, got, want)
			}
		}
		for id, want := range map[int]string{2: plainLabels, 4: brokenLabels, 5: "<null>"} {
			if got := column(db, "nodes", "labels", id); got != want {
				t.Errorf("node %d labels = %s, want it unchanged: %s", id, got, want)
			}
		}
		if got := column(db, "deployments", "placement", 1); got != reservedSelector {
			t.Errorf("deployment placement = %s, want it unchanged", got)
		}
		if got, want := attachments(t, db), []int{2, 4, 5}; !reflect.DeepEqual(got, want) {
			t.Errorf("attachments left = %v, want %v", got, want)
		}
		// Whatever survived is something a user may now write.
		for _, id := range []int{1, 3, 6} {
			survivors := map[string]string{}
			if err := json.Unmarshal([]byte(column(db, "nodes", "labels", id)), &survivors); err != nil {
				t.Fatal(err)
			}
			if err := nodelabel.ValidateWritten(survivors); err != nil {
				t.Errorf("node %d labels still rejected: %v", id, err)
			}
		}
	}

	m := migrationByVersion(t, 119)
	if m.Name != "free_reserved_label_namespace" {
		t.Fatalf("migration 119 = %q", m.Name)
	}

	db := open("free-reserved-labels")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	check(t, db)
	var marker SchemaMigration
	if err := db.First(&marker, 119).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// A second start skips it; running it again by hand changes nothing.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	check(t, db)

	// A failure after the rewrites rolls the rows and the marker back together.
	rollback := open("free-reserved-labels-rollback")
	want := errors.New("injected label deletion failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, c := range []struct {
		table, column string
		id            int
		want          string
	}{
		{"nodes", "labels", 1, reservedLabels},
		{"nodes", "labels", 3, upperOnly},
	} {
		if got := column(rollback, c.table, c.column, c.id); got != c.want {
			t.Fatalf("%s.%s after the failed migration = %s, want %s", c.table, c.column, got, c.want)
		}
	}
	if got := attachments(t, rollback); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("attachments after the failed migration = %v, want all five", got)
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 119).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(t, rollback)
}

// A database without the tables the migration reads is left alone rather
// than failing the upgrade.
func TestFreeReservedLabelNamespaceWithoutTheTables(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:free-reserved-labels-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := freeReservedLabelNamespace(db); err != nil {
		t.Fatalf("empty database: %v", err)
	}
}
