package store

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestModelCasefoldMigrationMergesReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-casefold.db")
	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec("DROP INDEX IF EXISTS idx_model_source_id_casefold").Error; err != nil {
		t.Fatal(err)
	}
	keeper := Model{Source: "huggingface", SourceID: "Org/Model", Revision: "main", Name: "keeper"}
	duplicate := Model{Source: "HuggingFace", SourceID: "org/model", Revision: "main", Name: "duplicate"}
	if err := st.DB().Create(&keeper).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&duplicate).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&ModelFile{ModelID: duplicate.ID, NodeID: 3, State: "present"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&Deployment{ModelID: duplicate.ID, State: "stopped"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&Profile{Fingerprint: "casefold-profile", ModelID: duplicate.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Delete(&SchemaMigration{}, 46).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].ID != keeper.ID {
		t.Fatalf("models = %+v, err=%v", models, err)
	}
	for table, target := range map[string]any{"model_files": &ModelFile{}, "deployments": &Deployment{}, "profiles": &Profile{}} {
		var count int64
		if err := st.DB().Model(target).Where("model_id = ?", keeper.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s keeper references = %d, want 1", table, count)
		}
	}
	if err := st.DB().Create(&Model{Source: "huggingface", SourceID: "ORG/MODEL", Revision: "main"}).Error; err == nil {
		t.Fatal("case-insensitive unique index accepted a duplicate")
	}
}
