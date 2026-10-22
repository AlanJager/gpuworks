package store

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestModelScopeDefaultRevisionMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "model-revision.db")
	st, err := Open(dbPath, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	ms, err := st.UpsertModel(&Model{Source: "modelscope", SourceID: "org/ms-model", Revision: "main", Name: "ms"})
	if err != nil {
		t.Fatal(err)
	}
	hf, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "org/hf-model", Revision: "main", Name: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&ModelFile{ModelID: ms.ID, StorageID: st.DefaultLocalStorageID(), NodeID: 1, Revision: "main"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&ModelFile{ModelID: hf.ID, StorageID: st.DefaultLocalStorageID(), NodeID: 2, Revision: "main"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Delete(&SchemaMigration{}, 43).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dbPath, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gotMS, err := st.GetModel(ms.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotHF, err := st.GetModel(hf.ID)
	if err != nil {
		t.Fatal(err)
	}
	msFile, err := st.ModelFileByModelNode(ms.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	hfFile, err := st.ModelFileByModelNode(hf.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if gotMS.Revision != "" || msFile.Revision != "" {
		t.Fatalf("ModelScope revisions = model %q file %q, want empty", gotMS.Revision, msFile.Revision)
	}
	if gotHF.Revision != "main" || hfFile.Revision != "main" {
		t.Fatalf("HuggingFace revisions = model %q file %q, want main", gotHF.Revision, hfFile.Revision)
	}
}
