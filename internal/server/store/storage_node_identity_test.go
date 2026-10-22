package store

import "testing"

func TestStorageNodeOverrideMergesLegacyIdentityBeforePatch(t *testing.T) {
	f := newStorageFixture(t)
	first, last, changed := int64(100), int64(200), int64(50)
	legacy := []StorageNode{
		{StorageID: f.cache.ID, NodeID: f.n1.ID, Path: "/legacy/mount", CachePath: "/old/cache", CacheMaxBytes: &first, Existing: true},
		{StorageID: f.cache.ID, NodeID: f.n1.ID, LabelKey: "old", LabelValue: "selector", CachePath: "/effective/cache", CacheMaxBytes: &last, OverrideOnly: true},
		{StorageID: f.cache.ID, NodeID: f.n2.ID, CachePath: "/unrelated/cache", CacheMaxBytes: &last, OverrideOnly: true},
	}
	if err := f.st.db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{SetCacheMaxBytes: true, CacheMaxBytes: &changed}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.StorageNodes(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != legacy[0].ID || rows[0].OverrideOnly || rows[0].LabelKey != "" || rows[0].LabelValue != "" || rows[0].Path != "/legacy/mount" || rows[0].CachePath != "/effective/cache" || rows[0].Existing || rows[0].CacheMaxBytes == nil || *rows[0].CacheMaxBytes != changed {
		t.Fatalf("patch lost effective fields or retained duplicate identity: %+v", rows)
	}
	if rows[1].ID != legacy[2].ID || rows[1].CachePath != legacy[2].CachePath || !rows[1].OverrideOnly || rows[1].CacheMaxBytes == nil || *rows[1].CacheMaxBytes != last {
		t.Fatalf("unrelated override changed: %+v", rows[1])
	}
	empty := ""
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n1.ID}, StorageNodeOverridePatch{CachePath: &empty, SetCacheMaxBytes: true}); err != nil {
		t.Fatal(err)
	}
	eff, applies, err := f.st.EffectiveStorageForNode(f.cache, f.n1)
	if err != nil || !applies || eff.CachePath != f.cache.CachePath || eff.CacheMaxBytes != f.cache.CacheMaxBytes || eff.Path != "/legacy/mount" || eff.Existing {
		t.Fatalf("inheritance did not take effect: %+v applies=%v err=%v", eff, applies, err)
	}
}

func TestStorageTargetsUseOneNodeIdentity(t *testing.T) {
	f := newStorageFixture(t)
	cap := int64(200)
	targets := []StorageNode{
		{NodeID: f.n1.ID, Path: "/mount", Existing: true},
		{NodeID: f.n1.ID, LabelValue: "legacy", CachePath: "/cache", CacheMaxBytes: &cap, OverrideOnly: true},
		{LabelKey: "pool", LabelValue: "a"},
		{LabelKey: "pool", LabelValue: "a"},
	}
	if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Targets: &targets}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.st.StorageNodes(f.cache.ID)
	if err != nil || len(rows) != 2 || rows[0].OverrideOnly || rows[0].LabelValue != "" || rows[0].Path != "/mount" || rows[0].CachePath != "/cache" || rows[0].CacheMaxBytes == nil || *rows[0].CacheMaxBytes != cap || rows[0].Existing || rows[1].LabelKey != "pool" {
		t.Fatalf("node identity not canonical: %+v err=%v", rows, err)
	}
}
