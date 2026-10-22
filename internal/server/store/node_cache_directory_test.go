package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestNodeCacheEntriesUseEachNodesCurrentDirectory(t *testing.T) {
	f := newStorageFixture(t)
	dir := "/custom-cache"
	if _, err := f.st.PatchStorageNodeOverrides(f.cache.ID, []uint{f.n2.ID}, StorageNodeOverridePatch{CachePath: &dir}); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Node{f.n1, f.n2} {
		current := f.cache.CachePath
		if node.ID == f.n2.ID {
			current = dir
		}
		for i, path := range []string{current + "/model", current + "-old/model", "/history/model", current, current + "/../elsewhere/model"} {
			model, err := f.st.UpsertModel(&Model{Name: path, Source: "local", SourceID: path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: model.ID, StorageID: f.cache.ID, Path: path, State: proto.MFPresent}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: model.ID, StorageID: f.cache.ID, NodeID: node.ID, Path: path, State: proto.MFPresent, SizeBytes: int64(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := f.st.NodeCacheEntriesForNodes(f.cache.ID, []uint{f.n1.ID, f.n2.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Node{f.n1, f.n2} {
		got := entries[node.ID]
		if len(got) != 1 || got[0].SizeBytes != 1 || !got[0].Evictable() {
			t.Fatalf("node %d current directory entries: %+v", node.ID, got)
		}
	}
	files, err := f.st.ModelFilesInStorage(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	var historical int
	for _, file := range files {
		if file.NodeID != 0 {
			historical++
		}
	}
	if historical != 10 {
		t.Fatalf("historical rows changed: %d", historical)
	}
}

func TestNodeCacheEntriesReserveRelativeCopyUntilAgentReports(t *testing.T) {
	f := newStorageFixture(t)
	file, err := f.st.PrepareModelCopy(FileRef{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID}, "models/pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.db.Model(file).Update("size_bytes", 60).Error; err != nil {
		t.Fatal(err)
	}
	entries, err := f.st.NodeCacheEntries(f.cache.ID, f.n1.ID)
	if err != nil || len(entries) != 1 || entries[0].ID != file.ID || entries[0].SizeBytes != 60 || !entries[0].Transferring || entries[0].Evictable() {
		t.Fatalf("pending relative copy lost its budget: %+v %v", entries, err)
	}
}
