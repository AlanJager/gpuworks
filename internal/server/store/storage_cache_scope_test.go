package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStorageOverridesDoNotSelectNodes(t *testing.T) {
	f := newStorageFixture(t)
	all := false
	zero := int64(0)
	targets := []StorageNode{{LabelKey: "pool", LabelValue: "a"}, {NodeID: f.n1.ID, OverrideOnly: true, CacheMaxBytes: &zero}, {NodeID: f.n2.ID}}
	row, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &all, Targets: &targets})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.st.EffectiveStorageForNode(row, f.n1); ok {
		t.Fatal("override selected unmatched node")
	}
	f.n1, err = f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if eff, ok, _ := f.st.EffectiveStorageForNode(row, f.n1); !ok || eff.CacheMaxBytes != 0 {
		t.Fatalf("matched zero override: %+v %v", eff, ok)
	}
	f.n1, err = f.st.SetNodeLabels(f.n1.UUID, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.st.EffectiveStorageForNode(row, f.n1); ok {
		t.Fatal("override retained lost membership")
	}
	if _, ok, _ := f.st.EffectiveStorageForNode(row, f.n2); !ok {
		t.Fatal("legacy explicit target lost membership")
	}
}

func TestStorageNodeOverridePatchPreservesFieldsAndRevision(t *testing.T) {
	f := newStorageFixture(t)
	targets := []StorageNode{{NodeID: f.n1.ID, Path: "/mount", CachePath: "/cache", Existing: true}}
	before, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Targets: &targets})
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID, f.n2.ID}, StorageNodeOverridePatch{SetCacheMaxBytes: true, CacheMaxBytes: &zero}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.st.StorageNodes(f.cache.ID)
	if len(rows) != 2 || rows[0].OverrideOnly || rows[0].Path != "/mount" || rows[0].CachePath != "/cache" || !rows[0].Existing || !rows[1].OverrideOnly {
		t.Fatalf("identity/fields changed: %+v", rows)
	}
	nextPath := "/other"
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n2.ID}, StorageNodeOverridePatch{CachePath: &nextPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{SetCacheMaxBytes: true}); err != nil {
		t.Fatal(err)
	}
	rows, _ = f.st.StorageNodes(f.cache.ID)
	if rows[0].CacheMaxBytes != nil || rows[1].CacheMaxBytes == nil || *rows[1].CacheMaxBytes != 0 || rows[1].CachePath != nextPath {
		t.Fatalf("independent fields lost: %+v", rows)
	}
	name := "stale"
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Name: &name, ExpectedUpdatedAt: &before.UpdatedAt}); !errors.Is(err, ErrStorageConfigChanged) {
		t.Fatalf("stale edit accepted: %v", err)
	}
}

func TestStorageNodeOverridePatchIsAtomicAndProtectsReaders(t *testing.T) {
	f := newStorageFixture(t)
	addStorageCacheReader(t, f, proto.InstRunning, 0)
	path := "/moved"
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n2.ID, f.n1.ID}, StorageNodeOverridePatch{CachePath: &path}); !errors.Is(err, ErrStorageInUse) {
		t.Fatalf("reader move: %v", err)
	}
	if rows, _ := f.st.StorageNodes(f.cache.ID); len(rows) != 0 {
		t.Fatalf("partial write: %+v", rows)
	}
	zero := int64(0)
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{SetCacheMaxBytes: true, CacheMaxBytes: &zero}); err != nil {
		t.Fatal(err)
	}
}

func TestAutoadoptedPathDoesNotPinLabelMembership(t *testing.T) {
	f := newStorageFixture(t)
	f.n1, _ = f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "a"})
	all := false
	targets := []StorageNode{{LabelKey: "pool", LabelValue: "a"}}
	row, err := f.st.UpdateStorage(f.local.ID, StoragePatch{AllNodes: &all, Targets: &targets})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.EnsureNodeOwnModelsDir(f.n1, "/home/user/models"); err != nil {
		t.Fatal(err)
	}
	f.n1, _ = f.st.SetNodeLabels(f.n1.UUID, map[string]string{})
	if _, ok, _ := f.st.EffectiveStorageForNode(row, f.n1); ok {
		t.Fatal("auto path pinned node")
	}
}

func TestReadyStorageNodesDiscardStaleOutOfScopeReports(t *testing.T) {
	f := newStorageFixture(t)
	f.n1, _ = f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "a"})
	all := false
	targets := []StorageNode{{LabelKey: "pool", LabelValue: "a"}}
	row, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &all, Targets: &targets})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.db.Model(&Node{}).Where("id = ?", f.n1.ID).Update("state", NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: row.ID, State: proto.StorageStateReady, Writable: true})
	ids, err := f.st.ReadyStorageNodes(row.ID, true)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ready: %v %v", ids, err)
	}
	f.n1, _ = f.st.SetNodeLabels(f.n1.UUID, map[string]string{})
	ids, err = f.st.ReadyStorageNodes(row.ID, true)
	if err != nil || len(ids) != 0 {
		t.Fatalf("stale ready: %v %v", ids, err)
	}
	counts, err := f.st.StorageReadyCounts()
	if err != nil || counts[row.ID].Writable != 0 || counts[row.ID].Ready != 0 {
		t.Fatalf("stale counts: %+v %v", counts, err)
	}
	if id, err := f.st.HeartbeatNode(row); err != nil || id != 0 {
		t.Fatalf("stale heartbeat: %d %v", id, err)
	}
}

func TestStorageCapacityReportsDoNotInvalidateConfigurationRevision(t *testing.T) {
	f := newStorageFixture(t)
	initial, err := f.st.StorageByID(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateReady, TotalBytes: 10000, AvailableBytes: 5000, Writable: true})
	reported, err := f.st.StorageByID(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reported.UpdatedAt.Equal(initial.UpdatedAt) || reported.TotalBytes != 10000 || reported.AvailableBytes != 5000 {
		t.Fatalf("capacity changed config revision: %+v -> %+v", initial, reported)
	}
	cap := int64(300)
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{CacheMaxBytes: &cap, ExpectedUpdatedAt: &initial.UpdatedAt}); err != nil {
		t.Fatalf("capacity caused edit conflict: %v", err)
	}
}

func TestNodePathUsageObservedAtOnlyTracksDiskReports(t *testing.T) {
	f := newStorageFixture(t)
	if f.n1.PathUsageObservedAt != nil {
		t.Fatal("fresh node has a disk report timestamp")
	}
	if err := f.st.UpdateNodeStatus(f.n1.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Path: "/cache", Kind: proto.PathKindStorageCache, StorageID: f.cache.ID, TotalBytes: 1000, AvailableBytes: 500}}}); err != nil {
		t.Fatal(err)
	}
	observed, err := f.st.NodeByUUID(f.n1.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.PathUsageObservedAt == nil {
		t.Fatal("disk report has no timestamp")
	}
	if err := f.st.TouchNode(f.n1.UUID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateNodeStatus(f.n1.UUID, proto.NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := f.st.NodeByUUID(f.n1.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.PathUsageObservedAt == nil || !heartbeat.PathUsageObservedAt.Equal(*observed.PathUsageObservedAt) {
		t.Fatalf("heartbeat refreshed disk fact: %+v -> %+v", observed.PathUsageObservedAt, heartbeat.PathUsageObservedAt)
	}
}

func TestStorageNodeOverrideConcurrentIndependentFields(t *testing.T) {
	f := newStorageFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	cap := int64(25)
	dir := "/concurrent/cache"
	for _, patch := range []StorageNodeOverridePatch{{SetCacheMaxBytes: true, CacheMaxBytes: &cap}, {CachePath: &dir}} {
		go func(patch StorageNodeOverridePatch) {
			<-start
			_, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, patch)
			results <- err
		}(patch)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	row, err := f.st.StorageByID(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	eff, ok, err := f.st.EffectiveStorageForNode(row, f.n1)
	if err != nil || !ok || eff.CacheMaxBytes != cap || eff.CachePath != dir {
		t.Fatalf("independent update lost: %+v %v %v", eff, ok, err)
	}
}

func TestStorageAndNodeDeletionRemoveCacheOperations(t *testing.T) {
	for _, removeNode := range []bool{false, true} {
		name := "storage"
		if removeNode {
			name = "node"
		}
		t.Run(name, func(t *testing.T) {
			f := newStorageFixture(t)
			if err := f.st.RequestCacheOperations(f.cache.ID, []uint{f.n1.ID}, "trim", true); err != nil {
				t.Fatal(err)
			}
			var err error
			if removeNode {
				err = f.st.DeleteNode(f.n1.UUID)
			} else {
				err = f.st.DeleteStorage(f.cache.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := f.st.CacheOperations(f.cache.ID); err != nil || len(rows) != 0 {
				t.Fatalf("orphan operations: %+v %v", rows, err)
			}
		})
	}
}
