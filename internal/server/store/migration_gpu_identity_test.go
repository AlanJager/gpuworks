package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// gpusSchemaAt116 is the gpus table exactly as store.Open created it at
// migration 116 (copied from sqlite_master), indexes included.
var gpusSchemaAt116 = []string{
	"CREATE TABLE `nodes` (`id` integer PRIMARY KEY AUTOINCREMENT,`uuid` text)",
	"CREATE TABLE `gpus` (`id` integer PRIMARY KEY AUTOINCREMENT,`node_id` integer,`index` integer,`uuid` text,`name` text,`compute_capability` text,`arch_family` text,`vendor` text,`arch_id` text,`memory_total` integer,`power_limit_watts` real,`pc_ie_bdf` text,`numa_node` integer,`mig_enabled` numeric,`healthy` numeric,`health_status` text,`health_code` text,`health_message` text,`created_at` datetime,`updated_at` datetime,CONSTRAINT `fk_nodes_gp_us` FOREIGN KEY (`node_id`) REFERENCES `nodes`(`id`) ON DELETE CASCADE)",
	"CREATE INDEX `idx_gpus_uuid` ON `gpus`(`uuid`)",
	"CREATE INDEX `idx_gpus_vendor` ON `gpus`(`vendor`)",
	"CREATE UNIQUE INDEX `idx_node_gpu` ON `gpus`(`node_id`,`index`)",
}

// gpuRowsAt116 are the rows the agents of each vendor left before 117: the
// names and arch columns as reported, every other column holding a value.
const gpuRowsAt116 = `INSERT INTO gpus (id, node_id, "index", uuid, name, compute_capability, arch_family, vendor, arch_id, memory_total, power_limit_watts, pc_ie_bdf, numa_node, mig_enabled, healthy, health_status, health_code, health_message, created_at, updated_at) VALUES
	(1, 1, 0, 'GPU-4090', 'NVIDIA GeForce RTX 4090', '8.9', 'Ada', 'nvidia', 'sm_89', 25757220864, 450, '00000000:01:00.0', 0, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(2, 1, 1, 'GPU-8000', 'Quadro RTX 8000', '7.5', 'Turing', 'nvidia', 'sm_75', 51539607552, 260, '00000000:02:00.0', 0, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(3, 2, 2, 'ascend-2', 'Ascend 910B4', '', '910B4', 'ascend', '910B4', 34359738368, 0, '0000:81:00.0', -1, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(4, 3, 0, 'ascend-0', 'Ascend Ascend910', '', 'Ascend910', 'ascend', 'Ascend910', 68719476736, 0, '0000:9D:00.0', -1, 0, 0, 'Warning', '80E18005', 'link down', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(5, 4, 0, 'GPU-k100', 'K100', '', 'K100_FAMILY_AMBIGUOUS', 'hygon', '', 68702699520, 0, '0000:19:00.0', 0, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(6, 4, 1, 'GPU-dcu', 'Hygon DCU', '', 'UNKNOWN_HYGON_SKU', 'hygon', '', 68702699520, 0, '0000:67:00.0', 1, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(7, 5, 0, 'GPU-ppu', 'PPU-ZW810E', '', 'Beethoven', 'ppu', 'ZW810E', 103079215104, 0, '0001:c9:00.0', 1, 0, 1, 'OK', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00'),
	(8, 6, 0, 'GPU-legacy', '  NVIDIA  A100 ', '', 'Ampere', '', 'sm_80', 85899345920, 400, '', -1, 1, 1, '', '', '', '2026-09-01 00:00:00', '2026-09-02 00:00:00')`

// gpuIdentityAt117 is what 117 leaves of each row: id → name, arch_id.
var gpuIdentityAt117 = map[int64][2]string{
	1: {"GeForce RTX 4090", ""},
	2: {"Quadro RTX 8000", ""},
	3: {"910B4", "910B4"},
	// Only the next report of an agent that knows the 910C layout makes
	// this a 910C; the migration does not guess from a name 910 shares.
	4: {"Ascend910", "Ascend910"},
	5: {"K100", "K100"},
	6: {"DCU", ""},
	7: {"PPU-ZW810E", "ZW810E"},
	// No capability: the sm_XY is the only architecture fact and stays.
	8: {"A100", "sm_80"},
}

func openGPUSchemaAt116(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(append([]string{}, gpusSchemaAt116...), `INSERT INTO nodes (id, uuid) VALUES (1, 'n1'), (2, 'n2'), (3, 'n3'), (4, 'n4'), (5, 'n5'), (6, 'n6')`, gpuRowsAt116) {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return db
}

func gpuIdentities(t *testing.T, db *gorm.DB) map[int64][2]string {
	t.Helper()
	got := map[int64][2]string{}
	for _, row := range tableRows(t, db, "gpus") {
		got[row["id"].(int64)] = [2]string{row["name"].(string), row["arch_id"].(string)}
	}
	return got
}

func TestGPUIdentityMigration(t *testing.T) {
	m := migrationByVersion(t, 117)
	db := openGPUSchemaAt116(t, "gpu-identity-migration")
	before, indexes := tableRows(t, db, "gpus"), indexNames(t, db, "gpus")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if columnsOf(t, db, "gpus")["arch_family"] {
		t.Fatal("gpus.arch_family survived")
	}
	if got := gpuIdentities(t, db); !reflect.DeepEqual(got, gpuIdentityAt117) {
		t.Fatalf("identities = %v, want %v", got, gpuIdentityAt117)
	}
	// Every other column keeps its value, and no index is lost.
	if got, want := withoutColumns(tableRows(t, db, "gpus"), "name", "arch_id"), withoutColumns(before, "name", "arch_id", "arch_family"); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if got := indexNames(t, db, "gpus"); !reflect.DeepEqual(got, indexes) {
		t.Fatalf("indexes = %v, want %v", got, indexes)
	}
	// The entity reads the migrated rows, and derives what was dropped.
	var gpu GPU
	if err := db.First(&gpu, 1).Error; err != nil {
		t.Fatal(err)
	}
	node := &Node{}
	if arch := node.ArchOf(gpu); arch.ID != "sm_89" || arch.Family != "ada" {
		t.Fatalf("4090 arch = %+v", arch)
	}
	var k100 GPU
	if err := db.First(&k100, 5).Error; err != nil {
		t.Fatal(err)
	}
	if arch := node.ArchOf(k100); arch.ID != "K100" || arch.Family != "k100" {
		t.Fatalf("K100 arch = %+v", arch)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 117).Error; err != nil || marker.Name != "gpu_identity_normalized" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	after := tableRows(t, db, "gpus")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// Running the body again on the finished schema is a no-op, not an error.
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying on the migrated schema: %v", err)
	}
	if got := tableRows(t, db, "gpus"); !reflect.DeepEqual(got, after) {
		t.Fatalf("re-applying changed rows: %v", got)
	}
}

func TestGPUIdentityMigrationRollsBackWithItsMarker(t *testing.T) {
	m := migrationByVersion(t, 117)
	db := openGPUSchemaAt116(t, "gpu-identity-rollback")
	before := tableRows(t, db, "gpus")
	want := errors.New("injected gpu identity failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if !columnsOf(t, db, "gpus")["arch_family"] {
		t.Fatal("the failed migration dropped arch_family")
	}
	if got := tableRows(t, db, "gpus"); !reflect.DeepEqual(got, before) {
		t.Fatalf("the failed migration changed rows: %v", got)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 117).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if columnsOf(t, db, "gpus")["arch_family"] || !reflect.DeepEqual(gpuIdentities(t, db), gpuIdentityAt117) {
		t.Fatal("restart after rollback did not migrate")
	}
}

func TestGPUIdentityMigrationSkipsAMissingTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:gpu-identity-no-table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 117)}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable("gpus") {
		t.Fatal("gpus was created")
	}
}

// An old agent's status report, as it arrives on the wire: names with the
// vendor prefix, the arch_family key, NVIDIA's sm_XY.
const oldAgentStatusJSON = `{
	"sys": {"hostname": "gpu3", "gpu_vendor": "nvidia", "gpu_driver": "550.54.15"},
	"gpus": [
		{"index": 0, "uuid": "GPU-a", "name": "NVIDIA  Quadro RTX 8000", "memory_total_bytes": 51539607552, "compute_capability": "7.5", "arch_family": "Turing", "vendor": "nvidia", "arch_id": "sm_75", "numa_node": 0, "healthy": true},
		{"index": 1, "uuid": "GPU-b", "name": "NVIDIA GeForce RTX 2080 Ti", "memory_total_bytes": 11811160064, "compute_capability": "7.5", "arch_family": "Turing", "vendor": "nvidia", "arch_id": "sm_75", "numa_node": 0, "healthy": true}
	]
}`

// The upgrade path as it runs in production: a database whose gpus table
// still has arch_family and agent-reported names, opened by a server that
// knows migration 117, restarted once more, and then sent an old agent's
// report.
func TestGPUIdentityMigrationSurvivesRestartAndOldAgentReports(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("upgrade-node", HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	// Put the database back to how 116 left it.
	for _, statement := range []string{
		"ALTER TABLE gpus ADD COLUMN arch_family text",
		"DELETE FROM schema_migrations WHERE version = 117",
	} {
		if err := st.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.db.Exec(`INSERT INTO gpus (node_id, "index", uuid, name, compute_capability, arch_family, vendor, arch_id, memory_total) VALUES (?, 0, 'GPU-a', 'NVIDIA Quadro RTX 8000', '7.5', 'Turing', 'nvidia', 'sm_75', 51539607552)`, node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		st, err = Open(dsn, bus.New())
		if err != nil {
			t.Fatalf("open %d: %v", restart, err)
		}
		if columnsOf(t, st.db, "gpus")["arch_family"] {
			t.Fatalf("open %d: arch_family survived", restart)
		}
		var markers int64
		if err := st.db.Model(&SchemaMigration{}).Where("version = ?", 117).Count(&markers).Error; err != nil || markers != 1 {
			t.Fatalf("open %d: marker count = %d err=%v", restart, markers, err)
		}
		n, err := st.NodeWithGPUs("upgrade-node")
		if err != nil || len(n.GPUs) != 1 || n.GPUs[0].Name != "Quadro RTX 8000" || n.GPUs[0].ArchID != "" {
			t.Fatalf("open %d: gpus = %+v err=%v", restart, n, err)
		}
		if restart == 0 {
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer st.Close()
	var status proto.NodeStatus
	if err := json.Unmarshal([]byte(oldAgentStatusJSON), &status); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("upgrade-node", status); err != nil {
		t.Fatal(err)
	}
	n, err := st.NodeWithGPUs("upgrade-node")
	if err != nil || len(n.GPUs) != 2 {
		t.Fatalf("gpus = %+v err=%v", n, err)
	}
	for i, wantName := range []string{"Quadro RTX 8000", "GeForce RTX 2080 Ti"} {
		g := n.GPUs[i]
		if g.Name != wantName || g.ArchID != "" || g.ComputeCapability != "7.5" {
			t.Fatalf("gpu %d = %+v", i, g)
		}
		if arch := n.ArchOf(g); arch.ID != "sm_75" || arch.Family != "turing" {
			t.Fatalf("gpu %d arch = %+v", i, arch)
		}
	}
}

// A new agent's report of every vendor, as it arrives on the wire.
const newAgentGPUsJSON = `[
	{"index": 0, "uuid": "ascend-0", "name": "Ascend 910C", "memory_total_bytes": 68719476736, "vendor": "ascend", "arch_id": "910C", "pcie_bdf": "0000:9D:00.0", "numa_node": -1, "healthy": true},
	{"index": 1, "uuid": "ascend-1", "name": "Ascend 910C", "memory_total_bytes": 68719476736, "vendor": "ascend", "arch_id": "910C", "pcie_bdf": "0000:9F:00.0", "numa_node": -1, "healthy": true},
	{"index": 2, "uuid": "GPU-k100", "name": "K100", "memory_total_bytes": 68702699520, "vendor": "hygon", "arch_id": "K100", "numa_node": 0, "healthy": true},
	{"index": 3, "uuid": "GPU-ppu", "name": "PPU-ZW810E", "memory_total_bytes": 103079215104, "vendor": "ppu", "arch_id": "ZW810E", "numa_node": 1, "healthy": true},
	{"index": 4, "uuid": "GPU-4090", "name": "NVIDIA GeForce RTX 4090", "memory_total_bytes": 25757220864, "compute_capability": "8.9", "vendor": "nvidia", "arch_id": "sm_89", "numa_node": 0, "healthy": true}
]`

func TestUpdateNodeStatusStoresNormalizedGPUIdentity(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("identity-node", HashToken("t"), "identity-node"); err != nil {
		t.Fatal(err)
	}
	var gpus []proto.GPUInfo
	if err := json.Unmarshal([]byte(newAgentGPUsJSON), &gpus); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("identity-node", proto.NodeStatus{GPUs: gpus}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NodeWithGPUs("identity-node")
	if err != nil || len(n.GPUs) != 5 {
		t.Fatalf("gpus = %+v err=%v", n, err)
	}
	want := []struct{ name, archID, id, family string }{
		{"910C", "910C", "910C", "910c"},
		{"910C", "910C", "910C", "910c"},
		{"K100", "K100", "K100", "k100"},
		{"PPU-ZW810E", "ZW810E", "ZW810E", "beethoven"},
		{"GeForce RTX 4090", "", "sm_89", "ada"},
	}
	for i, w := range want {
		g := n.GPUs[i]
		arch := n.ArchOf(g)
		if g.Name != w.name || g.ArchID != w.archID || arch.ID != w.id || arch.Family != w.family {
			t.Errorf("gpu %d = name %q arch_id %q arch %+v, want %+v", i, g.Name, g.ArchID, arch, w)
		}
	}
}
