package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRenameNetworkTypesMigration(t *testing.T) {
	const (
		rdmaRecord     = `{"explain":"fits","network":{"parameter":{"name":"ib","management":false,"type":"rdma","interface":"ib0","ip":"10.20.1.15","link_layer":"infiniband"}},"image":{"ref":"vllm:1"}}`
		ethernetRecord = `{"network":{"parameter":{"name":"management","management":true,"type":"ethernet","interface":"eth0","ip":"10.0.0.5"}}}`
		plainRecord    = `{"explain":"fits","image":{"ref":"vllm:1"}}`
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
			"CREATE TABLE networks (id INTEGER PRIMARY KEY, name TEXT, type TEXT)",
			"INSERT INTO networks (id, name, type) VALUES (1, 'storage', 'ethernet'), (2, 'ib', 'rdma'), (3, 'already', 'host_rdma')",
			"CREATE TABLE instances (id INTEGER PRIMARY KEY, schedule_expl TEXT)",
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		// Row 4 holds text that is not JSON and row 5 none at all, as an
		// instance that never recorded a plan does.
		for id, raw := range map[int]any{1: rdmaRecord, 2: ethernetRecord, 3: plainRecord, 4: "not json", 5: nil} {
			if err := db.Exec("INSERT INTO instances (id, schedule_expl) VALUES (?, ?)", id, raw).Error; err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	networkTypes := func(db *gorm.DB) []string {
		var types []string
		if err := db.Raw("SELECT type FROM networks ORDER BY id").Scan(&types).Error; err != nil {
			t.Fatal(err)
		}
		return types
	}
	record := func(db *gorm.DB, id int) string {
		var raw *string
		if err := db.Raw("SELECT schedule_expl FROM instances WHERE id = ?", id).Scan(&raw).Error; err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return "<null>"
		}
		return *raw
	}
	check := func(db *gorm.DB) {
		t.Helper()
		if got := networkTypes(db); len(got) != 3 || got[0] != "host_ethernet" || got[1] != "host_rdma" || got[2] != "host_rdma" {
			t.Fatalf("network types = %v", got)
		}
		// Only the type changes; every other key keeps its value and place.
		wantRDMA := `{"explain":"fits","network":{"parameter":{"name":"ib","management":false,"type":"host_rdma","interface":"ib0","ip":"10.20.1.15","link_layer":"infiniband"}},"image":{"ref":"vllm:1"}}`
		wantEthernet := `{"network":{"parameter":{"name":"management","management":true,"type":"host_ethernet","interface":"eth0","ip":"10.0.0.5"}}}`
		for id, want := range map[int]string{1: wantRDMA, 2: wantEthernet, 3: plainRecord, 4: "not json", 5: "<null>"} {
			if got := record(db, id); got != want {
				t.Fatalf("instance %d record = %s\nwant %s", id, got, want)
			}
		}
	}
	m := migrationByVersion(t, 111)
	if m.Name != "rename_network_types" {
		t.Fatalf("migration 111 = %q", m.Name)
	}

	db := open("rename-network-types")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	check(db)
	var marker SchemaMigration
	if err := db.First(&marker, 111).Error; err != nil || marker.Name != "rename_network_types" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// A second start skips it; applying it again by hand changes nothing.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	check(db)

	// A failure after the renames rolls back the rows and the marker together.
	rollback := open("rename-network-types-rollback")
	want := errors.New("injected rename failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if got := networkTypes(rollback); got[0] != "ethernet" || got[1] != "rdma" {
		t.Fatalf("network types after the failed migration = %v", got)
	}
	if got := record(rollback, 1); got != rdmaRecord {
		t.Fatalf("instance record after the failed migration = %s", got)
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 111).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(rollback)

	// A database without either table (a fresh install before AutoMigrate)
	// passes through.
	empty, err := gorm.Open(sqlite.Open("file:rename-network-types-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil {
		t.Fatalf("empty database: %v", err)
	}
}
