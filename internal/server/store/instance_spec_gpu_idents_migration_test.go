package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// legacySpecDB is a database as migration 143 left one placed before cards
// were bound by identity: the gpu_idents column back-filled, the desired
// state the agent is replayed still without identities. The rows are the
// shapes seen on a live server whose callable deployments (a single-node
// strata and a two-node leader + headless follower) had their own engines
// reported as processes outside the platform.
func legacySpecDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&SchemaMigration{}, &Instance{}); err != nil {
		t.Fatal(err)
	}
	// Written as raw JSON, the way an older server serialized it: no
	// gpu_idents key at all.
	rows := []struct{ uuid, idents, spec string }{
		{"strata", `["pci:0000:4b:00.0"]`,
			`{"uuid":"strata","deployment_id":15,"desired_state":"running","engine":"callable","image":"swr.example/strata:0.1","gpu_indexes":[1],"api_port":30001}`},
		{"leader", `["pci:000f:01:00.0"]`,
			`{"uuid":"leader","deployment_id":16,"desired_state":"running","engine":"callable","gpu_indexes":[0],"dist_role":"leader","nnodes":2}`},
		{"follower", `["pci:000f:01:00.0"]`,
			`{"uuid":"follower","deployment_id":16,"desired_state":"running","engine":"callable","gpu_indexes":[0],"dist_role":"follower","nnodes":2,"node_rank":1,"headless":true}`},
		// Already bound since launch: what it names stays.
		{"bound", `["pci:0000:32:00.0"]`,
			`{"uuid":"bound","desired_state":"running","engine":"vllm","gpu_indexes":[0],"gpu_idents":["pci:0000:32:00.0"]}`},
		// The column does not cover every index: a partial binding is worse
		// than none (see the agent's resolveBoundCards).
		{"partial", `["pci:0000:1a:00.0"]`,
			`{"uuid":"partial","desired_state":"running","gpu_indexes":[0,1]}`},
		// 143 found no identity for its card.
		{"unbound", ``,
			`{"uuid":"unbound","desired_state":"running","gpu_indexes":[3]}`},
		{"garbled", `["pci:0000:1a:00.0"]`, `{not json`},
	}
	for _, r := range rows {
		if err := db.Exec("INSERT INTO instances (uuid, state, gpu_idents, spec) VALUES (?, ?, ?, ?)",
			r.uuid, proto.InstRunning, r.idents, r.spec).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func specOf(t *testing.T, db *gorm.DB, uuid string) (string, proto.InstanceDesiredState) {
	t.Helper()
	var raw string
	if err := db.Raw("SELECT spec FROM instances WHERE uuid = ?", uuid).Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	var spec proto.InstanceDesiredState
	_ = json.Unmarshal([]byte(raw), &spec)
	return raw, spec
}

func TestInstanceSpecGPUIdentsMigrationHandsTheAgentTheBinding(t *testing.T) {
	db := legacySpecDB(t, "instance-spec-gpu-idents")
	before := map[string]string{}
	for _, uuid := range []string{"bound", "partial", "unbound", "garbled"} {
		before[uuid], _ = specOf(t, db, uuid)
	}
	m := migrationByName(t, "instance_spec_gpu_idents")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}

	for uuid, want := range map[string]string{
		"strata":   "pci:0000:4b:00.0",
		"leader":   "pci:000f:01:00.0",
		"follower": "pci:000f:01:00.0",
	} {
		_, spec := specOf(t, db, uuid)
		if len(spec.GPUIdents) != 1 || spec.GPUIdents[0] != want {
			t.Fatalf("%s gpu_idents = %v, want [%s]", uuid, spec.GPUIdents, want)
		}
		if len(spec.GPUIndexes) != 1 || spec.Engine != "callable" || spec.DesiredState != "running" {
			t.Fatalf("%s lost the rest of its desired state: %+v", uuid, spec)
		}
	}
	_, follower := specOf(t, db, "follower")
	if !follower.Headless || follower.NodeRank != 1 || follower.DistRole != "follower" {
		t.Fatalf("follower topology = %+v", follower)
	}
	_, strata := specOf(t, db, "strata")
	if strata.Image != "swr.example/strata:0.1" || strata.APIPort != 30001 || strata.DeploymentID != 15 {
		t.Fatalf("strata spec = %+v", strata)
	}
	for uuid, raw := range before {
		if got, _ := specOf(t, db, uuid); got != raw {
			t.Fatalf("%s spec changed:\n got %s\nwant %s", uuid, got, raw)
		}
	}

	var marker SchemaMigration
	if err := db.Where("version = ?", m.Version).First(&marker).Error; err != nil || marker.Name != "instance_spec_gpu_idents" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// Run again with no marker, as after a restore: nothing moves.
	after, _ := specOf(t, db, "strata")
	if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun err = %v", err)
	}
	if again, _ := specOf(t, db, "strata"); again != after {
		t.Fatalf("rerun rewrote the spec:\n got %s\nwant %s", again, after)
	}
}

func TestInstanceSpecGPUIdentsMigrationRollsBackWithItsMarker(t *testing.T) {
	db := legacySpecDB(t, "instance-spec-gpu-idents-rollback")
	m := migrationByName(t, "instance_spec_gpu_idents")
	boom := errors.New("injected")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if _, spec := specOf(t, db, "strata"); len(spec.GPUIdents) != 0 {
		t.Fatalf("a failed migration left gpu_idents %v", spec.GPUIdents)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers = %d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if _, spec := specOf(t, db, "strata"); len(spec.GPUIdents) != 1 {
		t.Fatalf("after the rerun gpu_idents = %v", spec.GPUIdents)
	}
}

// A schema built from scratch (or one older than the column) has nothing to
// copy and must not fail.
func TestInstanceSpecGPUIdentsMigrationSkipsAMissingColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:instance-spec-gpu-idents-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByName(t, "instance_spec_gpu_idents")}); err != nil {
		t.Fatalf("no instances table: %v", err)
	}
	if err := db.AutoMigrate(&Instance{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE instances DROP COLUMN gpu_idents").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&SchemaMigration{}, 173).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByName(t, "instance_spec_gpu_idents")}); err != nil {
		t.Fatalf("no gpu_idents column: %v", err)
	}
}

// The whole chain an upgraded server runs: an instance from before cards
// were bound, 143's column back-fill, then this migration, then the full
// desired state an agent is replayed when it reconnects — which is the only
// place the agent learns which cards its instances hold.
func TestUpgradedServerReplaysTheBindingToTheAgent(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("legacy-node", HashToken("t"), "legacy-host")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{card(0, "0000:32:00.0"), card(1, "0000:4b:00.0")}
	if err := s.UpdateNodeStatus("legacy-node", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards}); err != nil {
		t.Fatal(err)
	}
	// Launched before the binding: neither the column nor the spec has it.
	instanceRow(t, s, "legacy-strata", node.ID, []int{1}, nil)
	db := s.DB()
	for _, name := range []string{"instance_gpu_idents", "instance_spec_gpu_idents"} {
		m := migrationByName(t, name)
		if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
			t.Fatal(err)
		}
	}
	replayed := func() proto.InstanceDesiredState {
		t.Helper()
		ds, err := s.DesiredStateForNode(node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ds.Instances) != 1 {
			t.Fatalf("replayed instances = %+v", ds.Instances)
		}
		return ds.Instances[0]
	}

	if err := migrateDB(db, []migration{migrationByName(t, "instance_gpu_idents")}); err != nil {
		t.Fatal(err)
	}
	if got := instanceByUUID(t, s, "legacy-strata").GPUIdents; got != `["pci:0000:4b:00.0"]` {
		t.Fatalf("143 back-filled %q", got)
	}
	if spec := replayed(); len(spec.GPUIdents) != 0 {
		t.Fatalf("143 alone already replays %v; this migration would not be needed", spec.GPUIdents)
	}

	if err := migrateDB(db, []migration{migrationByName(t, "instance_spec_gpu_idents")}); err != nil {
		t.Fatal(err)
	}
	spec := replayed()
	if len(spec.GPUIdents) != 1 || spec.GPUIdents[0] != "pci:0000:4b:00.0" || len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 1 {
		t.Fatalf("replayed spec binds %v at %v, want pci:0000:4b:00.0 at device 1", spec.GPUIdents, spec.GPUIndexes)
	}
}
