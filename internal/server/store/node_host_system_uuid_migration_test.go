package store

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// The machine's firmware UUID arrives as one typed column, empty for every
// existing node until its agent's next hello. Applying the change again
// changes nothing and leaves one marker.
func TestNodeHostSystemUUIDMigrationAddsItsColumn(t *testing.T) {
	db := nodesOnlyDB(t, "hostname text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, hostname) VALUES (1, 'n-1', 'gpu-1')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_host_system_uuid")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "host_system_uuid") {
		t.Fatal("host_system_uuid is missing after the migration")
	}
	var row struct {
		HostSystemUUID *string
		Hostname       string
	}
	if err := db.Raw(`SELECT host_system_uuid, hostname FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.HostSystemUUID == nil || *row.HostSystemUUID != "" || row.Hostname != "gpu-1" {
		t.Fatalf("existing node after the migration: uuid=%v hostname=%q (want a non-null empty value)", row.HostSystemUUID, row.Hostname)
	}
	var notNull int
	if err := db.Raw(`SELECT "notnull" FROM pragma_table_info('nodes') WHERE name = 'host_system_uuid'`).Scan(&notNull).Error; err != nil || notNull != 1 {
		t.Fatalf("host_system_uuid notnull = %d, error=%v; want NOT NULL", notNull, err)
	}
	if err := db.Exec(`INSERT INTO nodes (id, uuid) VALUES (2, 'n-2')`).Error; err != nil {
		t.Fatalf("a new row without the column must take the default: %v", err)
	}
	var fresh string
	if err := db.Raw(`SELECT host_system_uuid FROM nodes WHERE id = 2`).Scan(&fresh).Error; err != nil || fresh != "" {
		t.Fatalf("new row's host_system_uuid = %q, error=%v; want empty", fresh, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("re-running the migration list: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failure leaves neither, and the
// retry completes.
func TestNodeHostSystemUUIDMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_host_system_uuid")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_host_uuid_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "host_system_uuid") {
		t.Fatal("the column survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_host_uuid_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "host_system_uuid") {
		t.Fatal("the retry did not add the column")
	}
}

// The setter normalizes, writes only a change, and never erases a value
// with an empty or unusable one.
func TestSetNodeHostSystemUUID(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, _, err := st.CreateNodePending("host-uuid")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in      string
		changed bool
		want    string
	}{
		{"4C4C4544-0042-3510-8051-B4C04F4E3432", true, "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		{"4c4c4544-0042-3510-8051-b4c04f4e3432", false, "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		{"", false, "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		{"00000000-0000-0000-0000-000000000000", false, "4c4c4544-0042-3510-8051-b4c04f4e3432"},
		{"ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8", true, "ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"},
	} {
		changed, err := st.SetNodeHostSystemUUID(n.UUID, tc.in)
		if err != nil || changed != tc.changed {
			t.Fatalf("SetNodeHostSystemUUID(%q) = %v, %v; want changed=%v", tc.in, changed, err, tc.changed)
		}
		got, err := st.NodeByUUID(n.UUID)
		if err != nil || got.HostSystemUUID != tc.want {
			t.Fatalf("after %q stored %q (%v), want %q", tc.in, got.HostSystemUUID, err, tc.want)
		}
	}
	if changed, err := st.SetNodeHostSystemUUID("no-such-node", "ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e9"); err != nil || changed {
		t.Fatalf("unknown node = %v, %v", changed, err)
	}
}
