package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRenameComputeNetworkMigration(t *testing.T) {
	const (
		// As migration 111 left it: an RDMA rank and a management one.
		rdmaRecord       = `{"explain":"fits","breakdown":{"total":1},"image":{"ref":"vllm:1"},"pd":{"role":"prefill","address":"10.20.1.15","port":8998},"network":{"parameter":{"name":"ib","management":false,"type":"host_rdma","interface":"ib0","ip":"10.20.1.15","link_layer":"infiniband"}}}`
		managementRecord = `{"network":{"parameter":{"name":"management","management":true,"type":"host_ethernet","interface":"eth0","ip":"10.0.0.5"}}}`
		plainRecord      = `{"explain":"fits","image":{"ref":"vllm:1"}}`
		emptyNetwork     = `{"explain":"fits","network":{}}`
		renamedRecord    = `{"network":{"compute":{"name":"ib","management":false,"type":"host_rdma","interface":"ib0","ip":"10.20.1.15"}}}`

		// A desired state keeps the resolved launch, mounts included, under
		// "launch"; retired_key stands for a key a newer type no longer has.
		rankSpec  = `{"launch":{"argv":["--x","<&>"],"retired_key":{"kept":true},"mounts":[{"source":"/models/a","target":"/model","read_only":true},{"source":"/etc/hccn.conf","target":"/etc/hccn.conf","read_only":true,"purpose":"parameter_network"},{"target":"/mnt/kv","size_hint":"1024","purpose":"kv-disk"}]},"uuid":"i-1","image":"vllm:1","gpu_indexes":[0,1]}`
		plainSpec = `{"launch":{"mounts":[{"source":"/models/a","target":"/model"}],"env":{"NOTE":"parameter_network in text only"}},"uuid":"i-2","image":"vllm:1"}`
		badMounts = `{"launch":{"mounts":"not a list","purpose":"parameter_network"}}`
		noMounts  = `{"launch":{"mounts":[]}}`
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
			"CREATE TABLE networks (id INTEGER PRIMARY KEY, name TEXT, roles TEXT)",
			"CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, management_roles_off TEXT)",
			"CREATE TABLE instances (id INTEGER PRIMARY KEY, schedule_expl TEXT, spec TEXT)",
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		for id, roles := range map[int]any{
			1: `["storage_sync","parameter","serving"]`,
			2: `["parameter"]`,
			3: `["serving"]`,
			4: `[]`,
			5: nil,
			6: "not json",
			7: `["parameter","compute"]`,
		} {
			if err := db.Exec("INSERT INTO networks (id, name, roles) VALUES (?, ?, ?)", id, id, roles).Error; err != nil {
				t.Fatal(err)
			}
		}
		for id, off := range map[int]any{1: `["storage_sync","parameter"]`, 2: `["serving"]`, 3: "", 4: nil} {
			if err := db.Exec("INSERT INTO nodes (id, uuid, management_roles_off) VALUES (?, ?, ?)", id, id, off).Error; err != nil {
				t.Fatal(err)
			}
		}
		for id, cols := range map[int][2]any{
			1: {rdmaRecord, rankSpec},
			2: {managementRecord, plainSpec},
			3: {plainRecord, nil},
			4: {emptyNetwork, "not json"},
			5: {"not json", badMounts},
			6: {nil, ""},
			7: {renamedRecord, noMounts},
		} {
			if err := db.Exec("INSERT INTO instances (id, schedule_expl, spec) VALUES (?, ?, ?)", id, cols[0], cols[1]).Error; err != nil {
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
	// sameJSON compares by value: the rewrite keeps every value but not the
	// order of an object's keys.
	sameJSON := func(t *testing.T, got, want string) bool {
		t.Helper()
		var g, w any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatalf("want %s: %v", want, err)
		}
		return json.Unmarshal([]byte(got), &g) == nil && reflect.DeepEqual(g, w)
	}
	check := func(t *testing.T, db *gorm.DB) {
		t.Helper()
		for id, want := range map[int]string{
			1: `["storage_sync","compute","serving"]`,
			2: `["compute"]`,
			3: `["serving"]`,
			4: `[]`,
			5: "<null>",
			6: "not json",
			7: `["compute"]`,
		} {
			if got := column(db, "networks", "roles", id); got != want {
				t.Errorf("network %d roles = %s, want %s", id, got, want)
			}
		}
		for id, want := range map[int]string{1: `["storage_sync","compute"]`, 2: `["serving"]`, 3: "", 4: "<null>"} {
			if got := column(db, "nodes", "management_roles_off", id); got != want {
				t.Errorf("node %d roles off = %s, want %s", id, got, want)
			}
		}
		renamed := map[int]string{
			1: `{"explain":"fits","breakdown":{"total":1},"image":{"ref":"vllm:1"},"pd":{"role":"prefill","address":"10.20.1.15","port":8998},"network":{"compute":{"name":"ib","management":false,"type":"host_rdma","interface":"ib0","ip":"10.20.1.15","link_layer":"infiniband"}}}`,
			2: `{"network":{"compute":{"name":"management","management":true,"type":"host_ethernet","interface":"eth0","ip":"10.0.0.5"}}}`,
		}
		for id, want := range renamed {
			if got := column(db, "instances", "schedule_expl", id); !sameJSON(t, got, want) {
				t.Errorf("instance %d record = %s\nwant %s", id, got, want)
			}
		}
		// Rows the rename does not concern keep their exact text.
		for id, want := range map[int]string{3: plainRecord, 4: emptyNetwork, 5: "not json", 6: "<null>", 7: renamedRecord} {
			if got := column(db, "instances", "schedule_expl", id); got != want {
				t.Errorf("instance %d record = %s, want it unchanged: %s", id, got, want)
			}
		}
		wantSpec := `{"launch":{"argv":["--x","<&>"],"retired_key":{"kept":true},"mounts":[{"source":"/models/a","target":"/model","read_only":true},{"source":"/etc/hccn.conf","target":"/etc/hccn.conf","read_only":true,"purpose":"compute_network"},{"target":"/mnt/kv","size_hint":"1024","purpose":"kv-disk"}]},"uuid":"i-1","image":"vllm:1","gpu_indexes":[0,1]}`
		if got := column(db, "instances", "spec", 1); !sameJSON(t, got, wantSpec) {
			t.Errorf("instance 1 spec = %s\nwant %s", got, wantSpec)
		}
		for id, want := range map[int]string{2: plainSpec, 3: "<null>", 4: "not json", 5: badMounts, 6: "", 7: noMounts} {
			if got := column(db, "instances", "spec", id); got != want {
				t.Errorf("instance %d spec = %s, want it unchanged: %s", id, got, want)
			}
		}
	}
	m := migrationByVersion(t, 118)
	if m.Name != "rename_parameter_network_to_compute" {
		t.Fatalf("migration 118 = %q", m.Name)
	}

	db := open("rename-compute-network")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	check(t, db)
	var marker SchemaMigration
	if err := db.First(&marker, 118).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// A second start skips it; applying it again by hand changes nothing.
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	check(t, db)

	// A failure after the rewrites rolls back the rows and the marker together.
	rollback := open("rename-compute-network-rollback")
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
	for _, c := range []struct {
		table, column, want string
	}{
		{"networks", "roles", `["storage_sync","parameter","serving"]`},
		{"nodes", "management_roles_off", `["storage_sync","parameter"]`},
		{"instances", "schedule_expl", rdmaRecord},
		{"instances", "spec", rankSpec},
	} {
		if got := column(rollback, c.table, c.column, 1); got != c.want {
			t.Fatalf("%s.%s after the failed migration = %s, want %s", c.table, c.column, got, c.want)
		}
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 118).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	check(t, rollback)

	// A database without the tables (a fresh install before AutoMigrate), or
	// with tables from before the columns, passes through.
	empty, err := gorm.Open(sqlite.Open("file:rename-compute-network-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil {
		t.Fatalf("empty database: %v", err)
	}
	old, err := gorm.Open(sqlite.Open("file:rename-compute-network-old?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE networks (id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)",
		"CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)",
	} {
		if err := old.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(old, []migration{m}); err != nil {
		t.Fatalf("tables without the columns: %v", err)
	}
}

// After the network types were renamed (migration 111) and the role (118),
// a record written before both reads back as the current shape.
func TestRenameMigrationsChainOnARecordFromBeforeBoth(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:rename-compute-network-chain?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE networks (id INTEGER PRIMARY KEY, name TEXT, type TEXT, roles TEXT)",
		`INSERT INTO networks (id, name, type, roles) VALUES (1, 'ib', 'rdma', '["parameter"]')`,
		"CREATE TABLE instances (id INTEGER PRIMARY KEY, schedule_expl TEXT)",
		`INSERT INTO instances (id, schedule_expl) VALUES (1, '{"network":{"parameter":{"name":"ib","type":"rdma","ip":"10.20.1.15"}}}')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 111), migrationByVersion(t, 118)}); err != nil {
		t.Fatal(err)
	}
	var network struct {
		Type, Roles string
	}
	if err := db.Raw("SELECT type, roles FROM networks WHERE id = 1").Scan(&network).Error; err != nil {
		t.Fatal(err)
	}
	if network.Type != "host_rdma" || network.Roles != `["compute"]` {
		t.Fatalf("network = %+v", network)
	}
	var record string
	if err := db.Raw("SELECT schedule_expl FROM instances WHERE id = 1").Scan(&record).Error; err != nil {
		t.Fatal(err)
	}
	if want := `{"network":{"compute":{"name":"ib","type":"host_rdma","ip":"10.20.1.15"}}}`; record != want {
		t.Fatalf("record = %s, want %s", record, want)
	}
}
