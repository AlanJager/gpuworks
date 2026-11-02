package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// Configs and model-card headers captured from the reported NFS library.
func TestDirectoryModelOperationInference(t *testing.T) {
	body, err := os.ReadFile("testdata/model_operation_metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name, Config, README, Want string
	}
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "models.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("node", store.HashToken("token"), "node")
			if err != nil {
				t.Fatal(err)
			}
			h := apiHandlers{d: Deps{Store: st, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
				if uuid != node.UUID {
					t.Fatalf("node = %q", uuid)
				}
				switch method {
				case proto.RPCRegisterPath:
					tasks := modelmetadata.InferTasks(modelmetadata.Documents{Config: fixture.Config, README: fixture.README})
					return json.Marshal(registerPathResponse{Path: "/models/artifact", SizeBytes: 123, Metadata: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: ".", Format: "transformers"}}, ConfigRaw: fixture.Config, README: fixture.README, Tasks: tasks.Tasks}})
				case proto.RPCFSReadFile:
					path := params.(map[string]any)["path"].(string)
					var content string
					switch filepath.Base(path) {
					case "config.json":
						content = fixture.Config
					case "README.md":
						content = fixture.README
					default:
						t.Fatalf("unexpected file %s", path)
					}
					return json.Marshal(localFileResponse{Path: path, Content: content, SizeBytes: int64(len(content))})
				default:
					t.Fatalf("unexpected RPC %s", method)
					return nil, nil
				}
			}}}
			preview, err := h.buildLocalModelPreview(context.Background(), node.UUID, "/models/artifact")
			if err != nil {
				t.Fatal(err)
			}
			assertOperation := func(label string, operations []inferenceapi.Operation) {
				t.Helper()
				if len(operations) != 1 || operations[0].Type != fixture.Want {
					t.Errorf("%s operations = %+v, want %s", label, operations, fixture.Want)
				}
			}
			assertOperation("local preview", preview.Operations)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(`{"node_uuid":"node","path":"/models/artifact"}`))
			h.registerLocalModel(recorder, request)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("local registration = %d %s", recorder.Code, recorder.Body.String())
			}
			var registered dto.Model
			if err := json.Unmarshal(recorder.Body.Bytes(), &registered); err != nil {
				t.Fatal(err)
			}
			assertOperation("local registration", registered.Operations)
			item := dto.StorageImportItem{RelPath: "artifact", Source: "huggingface", SourceID: fixture.Name, SizeBytes: 123}
			dir, err := h.modelDirFromMetadata(context.Background(), node.UUID, "/models/artifact", nil)
			if err != nil {
				t.Fatal(err)
			}
			imported, err := h.modelFromParsedDir(dir, item)
			if err != nil {
				t.Fatal(err)
			}
			var operations []inferenceapi.Operation
			if err := json.Unmarshal([]byte(imported.Operations), &operations); err != nil {
				t.Fatal(err)
			}
			assertOperation("NFS import persisted", operations)
			const edited = `[{"type":"openai.audio.speech"}]`
			if err := st.DB().Model(imported).Update("operations", edited).Error; err != nil {
				t.Fatal(err)
			}
			imported, err = h.modelFromParsedDir(dir, item)
			if err != nil || imported.Operations != edited {
				t.Fatalf("reimport overwrote contract: model=%+v err=%v", imported, err)
			}
			entity := &store.Model{Meta: fixture.Config, README: fixture.README}
			initRegisteredModelContract(entity)
			if err := json.Unmarshal([]byte(entity.Operations), &operations); err != nil {
				t.Fatal(err)
			}
			assertOperation("repository registration", operations)
			entity.Operations = `[{"type":"openai.audio.speech"}]`
			initRegisteredModelContract(entity)
			if entity.Operations != `[{"type":"openai.audio.speech"}]` {
				t.Fatal("overwrote an explicit operation contract")
			}
		})
	}
}
