package store

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestModelFilesForModelsScopesTheBatch(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: "other/model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, modelID := range []uint{f.model.ID, other.ID} {
		if _, err := f.st.UpsertModelFile(&ModelFile{ModelID: modelID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/models/weights", State: proto.MFPresent}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ids := range [][]uint{nil, {f.model.ID}, {f.model.ID, other.ID}} {
		for _, nodeID := range []uint{0, f.n1.ID, f.n2.ID} {
			files, err := f.st.ModelFilesForModels(ids, nodeID)
			if err != nil || len(files) != len(ids) {
				t.Fatalf("ids=%v node=%d files=%+v err=%v", ids, nodeID, files, err)
			}
			if len(ids) == 1 && files[0].ModelID != f.model.ID {
				t.Fatalf("unrelated model leaked into selected batch: %+v", files)
			}
		}
	}
}

func TestModelFilesForModelsScopesNodeCachesButKeepsLocalPeers(t *testing.T) {
	f := newSourceFixture(t)
	cache, err := f.st.CreateStorage(&Storage{Name: "cache", Type: proto.StorageNFSCache, Enabled: true,
		AllNodes: true, Path: "/mnt/cache", Export: "nfs.example.internal:/cache", CachePath: "/cache/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := f.file(t, cache, nil, "weights", proto.MFFailed)
	first := f.file(t, cache, f.n1, "/cache/models/weights", proto.MFPresent)
	second := f.file(t, cache, f.n2, "/cache/models/weights", proto.MFDownloading)
	local := f.file(t, f.local, f.n1, "/models/weights", proto.MFFailed)
	peer := f.file(t, f.local, f.n2, "/models/weights", proto.MFPresent)
	for _, tc := range []struct {
		name string
		node uint
		want []uint
	}{
		{"first node", f.n1.ID, []uint{shared.ID, first.ID, local.ID, peer.ID}},
		{"second node", f.n2.ID, []uint{shared.ID, second.ID, local.ID, peer.ID}},
		{"no target", 0, []uint{shared.ID, first.ID, second.ID, local.ID, peer.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := f.st.ModelFilesForModels([]uint{f.model.ID}, tc.node)
			if err != nil {
				t.Fatal(err)
			}
			var got []uint
			for _, file := range files {
				got = append(got, file.ID)
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("node-scoped files=%v, want shared/own/local-peer files=%v", got, tc.want)
			}
		})
	}
}

func TestListModelsForSourceIDs(t *testing.T) {
	f := newStorageFixture(t)
	other, err := f.st.UpsertModel(&Model{Source: "huggingface", SourceID: "other/model"})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := f.st.ListModels(" " + strings.ToUpper(f.model.SourceID) + " ")
	if err != nil || len(filtered) != 1 || filtered[0].ID != f.model.ID {
		t.Fatalf("repository selection includes unrelated models: models=%+v err=%v", filtered, err)
	}
	all, err := f.st.ListModels()
	if err != nil || len(all) != 2 || all[0].ID != other.ID {
		t.Fatalf("unfiltered newest-first listing changed: models=%+v err=%v", all, err)
	}
	missing, err := f.st.ListModels("missing/model")
	if err != nil || len(missing) != 0 {
		t.Fatalf("unknown repository returned models: models=%+v err=%v", missing, err)
	}
}
