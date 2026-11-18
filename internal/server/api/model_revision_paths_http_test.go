package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelHTTPDownloadsKeepDefaultAndExplicitRevisionPathsSeparate(t *testing.T) {
	r := newTokenRig(t, func(d *Deps) {
		d.FetchModelMeta = func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
			return &modelmeta.Meta{}, nil // An unpinned registration stays valid when upstream has no commit.
		}
	})
	t.Cleanup(func() { _ = r.st.Close() })
	node, err := r.st.RegisterNode("revision-node", store.HashToken("test-token"), "revision-node")
	if err != nil {
		t.Fatal(err)
	}
	models := make([]*store.Model, 0, 2)
	for _, revision := range []string{"", "main"} {
		created := r.do(r.session, http.MethodPost, "/api/v1/models", fmt.Sprintf(`{"source":"huggingface","source_id":"org/revision-model","revision":%q}`, revision))
		if created.Code != http.StatusCreated {
			t.Fatalf("register revision %q: %d %s", revision, created.Code, created.Body.String())
		}
		var model struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &model); err != nil {
			t.Fatal(err)
		}
		row, err := r.st.GetModel(model.ID)
		if err != nil || row.Revision != revision {
			t.Fatalf("registered identity=%+v err=%v, want revision %q", row, err, revision)
		}
		models = append(models, row)
		response := r.do(r.session, http.MethodPost, fmt.Sprintf("/api/v1/models/%d/download", model.ID), `{"node_uuid":"revision-node"}`)
		if response.Code != http.StatusAccepted {
			t.Fatalf("download revision %q: %d %s", revision, response.Code, response.Body.String())
		}
	}
	if models[0].ID == models[1].ID {
		t.Fatal("default and explicitly selected branch lost their separate identities")
	}
	first, err := r.st.ModelFileByModelNode(models[0].ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.st.ModelFileByModelNode(models[1].ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatalf("different model identities download into the same node directory %q", first.Path)
	}
	if first.Path != store.ModelArtifactPath(models[0]) {
		t.Fatalf("an uncontested existing layout moved: %q", first.Path)
	}
}

func TestModelHTTPDownloadRejectsOccupiedAllocatedDirectory(t *testing.T) {
	r := newTokenRig(t, func(d *Deps) {
		d.NodeSync = nodeSyncCallbacks{full: func(string) error { t.Error("conflicting download was dispatched"); return nil }}
	})
	t.Cleanup(func() { _ = r.st.Close() })
	node, err := r.st.RegisterNode("occupied-node", store.HashToken("test-token"), "occupied-node")
	if err != nil {
		t.Fatal(err)
	}
	var models []*store.Model
	for _, revision := range []string{"", "main", "other"} {
		model, err := r.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/occupied", Revision: revision})
		if err != nil {
			t.Fatal(err)
		}
		models = append(models, model)
	}
	base := store.ModelArtifactPath(models[0])
	allocated := fmt.Sprintf("%s-model-%d", base, models[1].ID)
	storageID := r.st.DefaultLocalStorageID()
	for _, owner := range []struct {
		modelID uint
		path    string
	}{{models[0].ID, base}, {models[2].ID, allocated}} {
		if _, err := r.st.RegisterModelFile(store.FileRef{ModelID: owner.modelID, StorageID: storageID, NodeID: node.ID}, "/models/"+owner.path, 19); err != nil {
			t.Fatal(err)
		}
	}
	response := r.do(r.session, http.MethodPost, fmt.Sprintf("/api/v1/models/%d/download", models[1].ID), `{"node_uuid":"occupied-node"}`)
	var body struct {
		Code   string `json:"code"`
		Params struct {
			ModelID   uint   `json:"model_id"`
			StorageID uint   `json:"storage_id"`
			Path      string `json:"path"`
		} `json:"params"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || body.Code != "MODEL_FILE_PATH_OCCUPIED" || body.Params.ModelID != models[2].ID || body.Params.StorageID != storageID || body.Params.Path != allocated {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if row, err := r.st.ModelFileByModelNode(models[1].ID, node.ID); err != store.ErrNotFound {
		t.Fatalf("refused download changed target: %+v %v", row, err)
	}
	for _, owner := range []*store.Model{models[0], models[2]} {
		row, err := r.st.ModelFileByModelNode(owner.ID, node.ID)
		if err != nil || row.State != proto.MFRegistered || row.SizeBytes != 19 {
			t.Fatalf("owner changed: %+v %v", row, err)
		}
	}
}

func TestModelHTTPSyncReportsAllocatedDirectoryConflict(t *testing.T) {
	r := newTokenRig(t, func(d *Deps) {
		d.ModelTransfers = &fakeTransfers{sync: func(modelID uint, target, source string, rate int) (*store.ModelFile, error) {
			if modelID != 42 || target != "target" || source != "source" || rate != -1 {
				t.Fatalf("unexpected sync: %d %q %q %d", modelID, target, source, rate)
			}
			return nil, &store.ModelFilePathOccupiedError{StorageID: 3, Path: "weights-model-42", ModelID: 43}
		}}
	})
	t.Cleanup(func() { _ = r.st.Close() })
	response := r.do(r.session, http.MethodPost, "/api/v1/models/42/nodes/target/sync", `{"source_node":"source"}`)
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || body.Code != "MODEL_FILE_PATH_OCCUPIED" || len(body.Params) != 3 || body.Params["path"] != "weights-model-42" || body.Params["model_id"] != float64(43) || body.Params["storage_id"] != float64(3) {
		t.Fatalf("sync response=%d %s", response.Code, response.Body.String())
	}
}
