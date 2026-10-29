package store

import (
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
		files, err := f.st.ModelFilesForModels(ids)
		if err != nil || len(files) != len(ids) {
			t.Fatalf("ids=%v files=%+v err=%v", ids, files, err)
		}
		if len(ids) == 1 && files[0].ModelID != f.model.ID {
			t.Fatalf("unrelated model leaked into selected batch: %+v", files)
		}
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
