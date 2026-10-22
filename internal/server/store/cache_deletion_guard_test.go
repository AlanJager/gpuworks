package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func pendingDeletionFixture(t *testing.T) (*storageFixture, *ModelFile) {
	t.Helper()
	f := newStorageFixture(t)
	file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, NodeID: f.n1.ID, StorageID: f.cache.ID, Path: f.cache.CachePath + "/model", State: proto.MFDeleting, TransferID: "unconfirmed-removal"})
	if err != nil {
		t.Fatal(err)
	}
	return f, file
}

func TestPendingCacheDeletionProtectsParentsAndStorageConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*storageFixture) error
	}{
		{"delete model", func(f *storageFixture) error { return f.st.DeleteModel(f.model.ID) }},
		{"delete node", func(f *storageFixture) error { return f.st.DeleteNode(f.n1.UUID) }},
		{"delete storage", func(f *storageFixture) error { return f.st.DeleteStorage(f.cache.ID) }},
		{"direct node mount path", func(f *storageFixture) error { return f.st.SetStorageNodePath(f.cache.ID, f.n1.ID, "/other/mount") }},
		{"switch type", func(f *storageFixture) error {
			kind := proto.StorageNFS
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Type: &kind})
			return err
		}},
		{"disable", func(f *storageFixture) error {
			no := false
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Enabled: &no})
			return err
		}},
		{"default cache path", func(f *storageFixture) error {
			dir := "/other/cache"
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{CachePath: &dir})
			return err
		}},
		{"mount path", func(f *storageFixture) error {
			dir := "/other/mount"
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Path: &dir})
			return err
		}},
		{"mount options", func(f *storageFixture) error {
			options := "ro"
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{MountOptions: &options})
			return err
		}},
		{"scope", func(f *storageFixture) error {
			no := false
			targets := []StorageNode{{NodeID: f.n2.ID}}
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &no, Targets: &targets})
			return err
		}},
		{"node cache path", func(f *storageFixture) error {
			dir := "/other/cache"
			_, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{CachePath: &dir})
			return err
		}},
		{"node mount ownership", func(f *storageFixture) error {
			targets := []StorageNode{{NodeID: f.n1.ID, Existing: true}}
			_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Targets: &targets})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, file := pendingDeletionFixture(t)
			before, _ := f.st.StorageByID(f.cache.ID)
			if err := tc.change(f); !errors.Is(err, ErrCacheDeletionPending) {
				t.Fatalf("unconfirmed deletion did not block change: %v", err)
			}
			after, err := f.st.ModelFileByID(file.ID)
			if err != nil || after.State != proto.MFDeleting || after.TransferID != file.TransferID {
				t.Fatalf("deletion fence lost: %+v %v", after, err)
			}
			current, err := f.st.StorageByID(f.cache.ID)
			if err != nil || !current.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("rejected configuration was partially committed: %+v %v", current, err)
			}
			if err := f.st.CompleteCacheDeletion(file); err != nil {
				t.Fatal(err)
			}
			if err := tc.change(f); err != nil {
				t.Fatalf("confirmed removal still blocks change: %v", err)
			}
		})
	}
}

func TestPendingCacheDeletionAllowsBudgetAndUnrelatedChanges(t *testing.T) {
	f, file := pendingDeletionFixture(t)
	cap := int64(50)
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{CacheMaxBytes: &cap}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{SetCacheMaxBytes: true, CacheMaxBytes: &cap}); err != nil {
		t.Fatal(err)
	}
	dir := "/unrelated/cache"
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n2.ID}, StorageNodeOverridePatch{CachePath: &dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetNodeLabels(f.n1.UUID, map[string]string{"unrelated": "label"}); err != nil {
		t.Fatal(err)
	}
	after, err := f.st.ModelFileByID(file.ID)
	if err != nil || after.State != proto.MFDeleting || after.TransferID != file.TransferID {
		t.Fatalf("budget edit changed deletion intent: %+v %v", after, err)
	}
}

func TestPendingCacheDeletionProtectsLabelMembership(t *testing.T) {
	f := newStorageFixture(t)
	if _, err := f.st.SetNodeLabels(f.n1.UUID, map[string]string{"pool": "cache"}); err != nil {
		t.Fatal(err)
	}
	no := false
	targets := []StorageNode{{LabelKey: "pool", LabelValue: "cache"}}
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &no, Targets: &targets}); err != nil {
		t.Fatal(err)
	}
	file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, NodeID: f.n1.ID, StorageID: f.cache.ID, Path: f.cache.CachePath + "/model", State: proto.MFDeleting, TransferID: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetNodeLabels(f.n1.UUID, map[string]string{}); !errors.Is(err, ErrCacheDeletionPending) {
		t.Fatalf("label edit removed pending deletion's storage: %v", err)
	}
	if err := f.st.CompleteCacheDeletion(file); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetNodeLabels(f.n1.UUID, map[string]string{}); err != nil {
		t.Fatal(err)
	}
}
