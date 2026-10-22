package store

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func addStorageCacheReader(t *testing.T, f *storageFixture, state string, sleepLevel int) *Instance {
	t.Helper()
	file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID,
		Path: "/node1/cache/model", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := f.st.CreateDeployment(&Deployment{Name: "reader", ServedName: "reader", ModelID: f.model.ID, NodeID: f.n1.ID})
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{UUID: "reader", DeploymentID: dep.ID, NodeID: f.n1.ID, State: state, SleepLevel: sleepLevel}
	if err := f.st.CreateInstances([]*Instance{inst}, [][]InstanceModelFile{{{ModelID: f.model.ID, ModelFileID: file.ID, HostPath: file.Path}}}); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestStorageTargetUpdatesKeepLiveReaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*storageFixture, []StorageNode) StoragePatch
		blocked bool
	}{
		{"add another node cache", func(f *storageFixture, targets []StorageNode) StoragePatch {
			targets = append(targets, StorageNode{NodeID: f.n2.ID, CachePath: "/node2/cache"})
			return StoragePatch{Targets: &targets}
		}, false},
		{"save unchanged targets", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			return StoragePatch{Targets: &targets}
		}, false},
		{"reorder targets", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets[0], targets[1] = targets[1], targets[0]
			return StoragePatch{Targets: &targets}
		}, false},
		{"trim paths before comparing", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets[0].Path, targets[0].CachePath = " /node1/mount ", " /node1/cache "
			return StoragePatch{Targets: &targets}
		}, false},
		{"ignore a later duplicate as persistence does", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			duplicate := targets[0]
			duplicate.CachePath = "/ignored"
			targets = append(targets, duplicate)
			return StoragePatch{Targets: &targets}
		}, false},
		{"change only cache quota", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			limit := int64(1024)
			targets[0].CacheMaxBytes = &limit
			return StoragePatch{Targets: &targets}
		}, false},
		{"narrow scope while retaining the reader", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			all := false
			return StoragePatch{AllNodes: &all, Targets: &targets}
		}, false},
		{"move the reader cache", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets[0].CachePath = "/moved/cache"
			return StoragePatch{Targets: &targets}
		}, true},
		{"move the reader mount", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets[0].Path = "/moved/mount"
			return StoragePatch{Targets: &targets}
		}, true},
		{"change the reader mount ownership", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets[0].Existing = false
			return StoragePatch{Targets: &targets}
		}, true},
		{"remove an override and fall back to different paths", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			targets = targets[1:]
			return StoragePatch{Targets: &targets}
		}, true},
		{"remove the reader from the storage", func(_ *storageFixture, targets []StorageNode) StoragePatch {
			all := false
			targets = targets[1:]
			return StoragePatch{AllNodes: &all, Targets: &targets}
		}, true},
		{"moving the global cache still requires idle", func(_ *storageFixture, _ []StorageNode) StoragePatch {
			path := "/other/global/cache"
			return StoragePatch{CachePath: &path}
		}, true},
		{"moving the global mount still requires idle", func(_ *storageFixture, _ []StorageNode) StoragePatch {
			path := "/other/global/mount"
			return StoragePatch{Path: &path}
		}, true},
		{"changing storage type still requires idle", func(_ *storageFixture, _ []StorageNode) StoragePatch {
			kind := proto.StorageNFS
			return StoragePatch{Type: &kind}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStorageFixture(t)
			targets := []StorageNode{{NodeID: f.n1.ID, Path: "/node1/mount", CachePath: "/node1/cache", Existing: true},
				{LabelKey: "pool", LabelValue: "other"}}
			if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Targets: &targets}); err != nil {
				t.Fatal(err)
			}
			addStorageCacheReader(t, f, proto.InstRunning, 0)
			before, err := f.st.StorageByID(f.cache.ID)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := f.st.StorageNodes(f.cache.ID)
			if err != nil {
				t.Fatal(err)
			}
			patch := tc.change(f, targets)
			name := "updated-cache"
			patch.Name = &name // A refused target must roll unrelated fields back too.
			_, err = f.st.UpdateStorage(f.cache.ID, patch)
			if tc.blocked {
				var inUse *StorageInUseError
				if !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.InstanceUUIDs, []string{"reader"}) {
					t.Fatalf("update = %v, want only the affected reader", err)
				}
				after, err := f.st.StorageByID(f.cache.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := f.st.StorageNodes(f.cache.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(previous, saved) {
					t.Fatalf("refused patch was not rolled back: storage=%+v targets=%+v", after, saved)
				}
			} else if err != nil {
				t.Fatalf("safe update refused: %v", err)
			}
			row, err := f.st.StorageByID(f.cache.ID)
			if err != nil {
				t.Fatal(err)
			}
			eff, applies, err := f.st.EffectiveStorageForNode(row, f.n1)
			if err != nil || !applies || eff.Path != "/node1/mount" || eff.CachePath != "/node1/cache" || !eff.Existing {
				t.Fatalf("reader configuration changed: %+v applies=%v err=%v", eff, applies, err)
			}
		})
	}
}

func TestStorageTargetChangesProtectUnfinishedReaders(t *testing.T) {
	for _, state := range []string{proto.InstStarting, proto.InstRunning, proto.InstRemoving, proto.InstStopped, proto.InstFailed} {
		t.Run(state, func(t *testing.T) {
			f := newStorageFixture(t)
			addStorageCacheReader(t, f, state, 1) // Sleeping instances still own their model files.
			targets := []StorageNode{{NodeID: f.n1.ID, CachePath: "/moved/cache"}}
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Targets: &targets})
			ended := state == proto.InstStopped || state == proto.InstFailed
			if ended && err != nil || !ended && !errors.Is(err, ErrStorageInUse) {
				t.Fatalf("state %s: update = %v", state, err)
			}
		})
	}
}

func TestStorageTargetUpdatesRespectEffectiveLabelScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		targets  []StorageNode
		allNodes bool
		blocked  bool
	}{
		{name: "another matching label retains access", targets: []StorageNode{{LabelKey: "zone", LabelValue: "a"}}},
		{name: "removing the only matching label removes access", targets: []StorageNode{{LabelKey: "pool", LabelValue: "other"}}, blocked: true},
		{name: "expanding to all nodes retains access", allNodes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStorageFixture(t)
			var err error
			f.n1, err = f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "reader", "zone": "a"})
			if err != nil {
				t.Fatal(err)
			}
			targets := []StorageNode{{LabelKey: "pool", LabelValue: "reader"}, {LabelKey: "zone", LabelValue: "a"}}
			all := false
			if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &all, Targets: &targets}); err != nil {
				t.Fatal(err)
			}
			addStorageCacheReader(t, f, proto.InstRunning, 0)
			_, err = f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &tc.allNodes, Targets: &tc.targets})
			if tc.blocked && !errors.Is(err, ErrStorageInUse) || !tc.blocked && err != nil {
				t.Fatalf("update = %v", err)
			}
		})
	}
}
