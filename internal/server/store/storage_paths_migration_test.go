package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

var pathsMigration = migration{Version: 126, Name: "storage_paths_explicit", Apply: migrateStoragePathsExplicit}

// pathsLegacyDB is a database after migrations 122-125: the default local
// storage leaves its directory to each agent and an nfs_cache storage leaves
// its cache directory to each agent's state directory. Node 1 has an override
// from its old models_root; nodes 2 and 3 reported the agent's own directory;
// node 4 never reported.
func pathsLegacyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := legacyStorageDB(t, true)
	if err := migrateDB(db, storageMigrations()); err != nil {
		t.Fatal(err)
	}
	cache := Storage{Name: "team-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Path: "/mnt/gpuworks/team-cache", Export: "nfs:/export/team"}
	if err := db.Create(&cache).Error; err != nil {
		t.Fatal(err)
	}
	if cache.ID != 2 {
		t.Fatalf("cache storage id = %d, want 2", cache.ID)
	}
	for _, st := range []StorageNodeState{
		{StorageID: 1, NodeID: 1, State: proto.StorageStateReady, Path: "/data/models"},
		{StorageID: 1, NodeID: 2, State: proto.StorageStateReady, Path: "/opt/gpuworks1/agent/models"},
		{StorageID: 1, NodeID: 3, State: proto.StorageStateReady, Path: "/home/renter/gpuworks/models"},
		{StorageID: 2, NodeID: 2, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-cache", CachePath: "/opt/gpuworks1/agent/cache/team-cache"},
	} {
		st := st
		if err := db.Create(&st).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Node 3 already carries an override row for something else (a cache cap
	// on the cache storage would look the same): its directory is filled in.
	limit := int64(5)
	if err := db.Create(&StorageNode{StorageID: 1, NodeID: 3, CacheMaxBytes: &limit}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func overridesOf(t *testing.T, db *gorm.DB, storageID uint) map[uint]StorageNode {
	t.Helper()
	var rows []StorageNode
	if err := db.Where("storage_id = ?", storageID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]StorageNode{}
	for _, r := range rows {
		out[r.NodeID] = r
	}
	return out
}

func TestStoragePathsMigrationKeepsEveryNodesDirectoryAndStatesTheDefault(t *testing.T) {
	db := pathsLegacyDB(t)
	if err := migrateDB(db, []migration{pathsMigration}); err != nil {
		t.Fatal(err)
	}
	var local, cache Storage
	if err := db.First(&local, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&cache, 2).Error; err != nil {
		t.Fatal(err)
	}
	if local.Path != DefaultLocalStoragePath || cache.CachePath != "/opt/gpuworks/cache/team-cache" || cache.Path != "/mnt/gpuworks/team-cache" {
		t.Fatalf("storages after = %+v / %+v", local, cache)
	}
	got := overridesOf(t, db, 1)
	want := map[uint]string{1: "/data/models", 2: "/opt/gpuworks1/agent/models", 3: "/home/renter/gpuworks/models"}
	if len(got) != len(want) {
		t.Fatalf("local overrides = %+v, want nodes 1-3 (node 4 never reported)", got)
	}
	for node, dir := range want {
		if got[node].Path != dir {
			t.Errorf("node %d override = %q, want %q", node, got[node].Path, dir)
		}
	}
	if got[3].CacheMaxBytes == nil || *got[3].CacheMaxBytes != 5 {
		t.Errorf("node 3's other override was lost: %+v", got[3])
	}
	cacheOverrides := overridesOf(t, db, 2)
	if len(cacheOverrides) != 1 || cacheOverrides[2].CachePath != "/opt/gpuworks1/agent/cache/team-cache" || cacheOverrides[2].Path != "" {
		t.Fatalf("cache overrides = %+v", cacheOverrides)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 126).First(&marker).Error; err != nil || marker.Name != "storage_paths_explicit" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	// A second run (no marker, as after a restore) changes nothing.
	if err := db.Transaction(migrateStoragePathsExplicit); err != nil {
		t.Fatal(err)
	}
	if again := overridesOf(t, db, 1); len(again) != 3 {
		t.Fatalf("second run: overrides = %+v", again)
	}
}

func TestStoragePathsMigrationRollsBackWithItsMarker(t *testing.T) {
	db := pathsLegacyDB(t)
	boom := errors.New("injected")
	failing := migration{Version: 126, Name: "storage_paths_explicit", Apply: func(tx *gorm.DB) error {
		if err := migrateStoragePathsExplicit(tx); err != nil {
			return err
		}
		return boom
	}}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	var local Storage
	if err := db.First(&local, 1).Error; err != nil || local.Path != "" {
		t.Fatalf("storage path after a failed migration = %q err=%v", local.Path, err)
	}
	if got := overridesOf(t, db, 1); len(got) != 2 {
		t.Fatalf("overrides after a failed migration = %+v, want the two from before", got)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 126).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers = %d err=%v", markers, err)
	}
	// Started again, it completes.
	if err := migrateDB(db, []migration{pathsMigration}); err != nil {
		t.Fatal(err)
	}
	if got := overridesOf(t, db, 1); len(got) != 3 {
		t.Fatalf("overrides after the rerun = %+v", got)
	}
}

// An agent without root offers its own directory on connect; it becomes the
// node's override once, and never replaces one the node already has.
func TestEnsureNodeOwnModelsDirOnlyFillsAMissingOverride(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	if err := st.EnsureNodeOwnModelsDir(f.n1, "/home/renter/gpuworks/models"); err != nil {
		t.Fatal(err)
	}
	eff, applies, err := st.EffectiveStorageForNode(f.local, f.n1)
	if err != nil || !applies || eff.Path != "/home/renter/gpuworks/models" {
		t.Fatalf("n1 effective = %+v applies=%v err=%v", eff, applies, err)
	}
	// An operator moved it; a later connect does not undo that.
	if err := st.SetStorageNodePath(f.local.ID, f.n1.ID, "/data/models"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureNodeOwnModelsDir(f.n1, "/home/renter/gpuworks/models"); err != nil {
		t.Fatal(err)
	}
	if eff, _, _ = st.EffectiveStorageForNode(f.local, f.n1); eff.Path != "/data/models" {
		t.Fatalf("operator's directory replaced: %q", eff.Path)
	}
	// Other nodes keep the storage's own directory.
	if eff, _, _ = st.EffectiveStorageForNode(f.local, f.n2); eff.Path != f.local.Path {
		t.Fatalf("n2 effective path = %q, want the storage's %q", eff.Path, f.local.Path)
	}
	if err := st.SetStorageNodePath(f.local.ID, f.n2.ID, "relative/dir"); err == nil {
		t.Fatal("a relative directory was accepted")
	}
}
