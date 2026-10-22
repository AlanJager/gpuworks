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

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

func TestLocalRegistrationRejectsKnownMissingShard(t *testing.T) {
	root := t.TempDir()
	for n, b := range map[string]string{"config.json": `{"architectures":["ExampleForCausalLM"]}`, "model.safetensors.index.json": `{"weight_map":{"a":"part-a.safetensors","b":"part-b.safetensors"}}`, "part-a.safetensors": "weights"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := modelscan.ParseMetadata(context.Background(), root)
	if err != nil || len(report.Entries) != 1 || !report.Entries[0].Incomplete {
		t.Fatalf("bad fixture %+v %v", report, err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("quality-review", store.HashToken("fixture-token"), "quality-review")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCRegisterPath {
			t.Fatalf("unexpected %s", method)
		}
		size, err := modelscan.ValidateModelDir(root)
		if err != nil {
			return nil, err
		}
		return json.Marshal(registerPathResponse{Path: root, SizeBytes: size, Metadata: &report})
	}}}
	body, _ := json.Marshal(map[string]string{"node_uuid": node.UUID, "path": root})
	w := httptest.NewRecorder()
	h.registerLocalModel(w, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(string(body))))
	shared, err := st.CreateStorage(&store.Storage{Name: "shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: filepath.Dir(root), Export: "example:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.importOne(context.Background(), shared, node, filepath.Dir(root), dto.StorageImportItem{RelPath: filepath.Base(root), Source: "local", SourceID: "incomplete"})
	if err == nil || !strings.Contains(err.Error(), "part-b.safetensors") {
		t.Fatalf("storage import lost missing shard: %v", err)
	}
	models, _ := st.ListModels()
	if w.Code != http.StatusBadRequest || len(models) != 0 || !strings.Contains(w.Body.String(), "part-b.safetensors") {
		t.Errorf("known incomplete report ignored: HTTP=%d persisted_models=%d missing=%v", w.Code, len(models), report.Entries[0].Missing)
	}
}

func TestImportAdmissionAcrossEntrypoints(t *testing.T) {
	for _, tc := range []struct {
		name               string
		report             *modelmetadata.Report
		localOK, storageOK bool
		cause              string
	}{
		{name: "complete", report: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: ".", Format: "transformers"}}}, localOK: true, storageOK: true},
		{name: "missing shard", report: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: ".", Incomplete: true, Missing: []string{"part-b.safetensors"}}}}, cause: "part-b.safetensors"},
		{name: "unreadable nested index", report: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: ".", Incomplete: true}}, Issues: []modelmetadata.Issue{{Path: "encoder/model.safetensors.index.json", Code: "metadata_unreadable", Detail: "unexpected EOF"}}}, cause: "unexpected EOF"},
		{name: "unknown component", report: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: ".", Incomplete: true}}, Issues: []modelmetadata.Issue{{Path: "encoder", Code: "unverified_component", Detail: "custom.Encoder"}}}, cause: "custom.Encoder"},
		{name: "component only", report: &modelmetadata.Report{Component: true}, cause: "component"},
		{name: "multiple entries", report: &modelmetadata.Report{Entries: []modelmetadata.Entry{{Path: "a"}, {Path: "b"}}}, cause: "exactly one"},
		{name: "unknown explicit layout", report: &modelmetadata.Report{}, localOK: true, cause: "exactly one"},
		{name: "older agent", localOK: true, storageOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entrance := range []string{"local", "storage"} {
				t.Run(entrance, func(t *testing.T) {
					st, err := store.Open(filepath.Join(t.TempDir(), "db"), bus.New())
					if err != nil {
						t.Fatal(err)
					}
					defer st.Close()
					node, err := st.RegisterNode("node", store.HashToken("token"), "node")
					if err != nil {
						t.Fatal(err)
					}
					root := t.TempDir()
					modelPath := filepath.Join(root, "model")
					shared, err := st.CreateStorage(&store.Storage{Name: "shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: root, Export: "example:/models"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					h := apiHandlers{d: Deps{Store: st, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
						if uuid != node.UUID {
							t.Fatalf("wrong node %s", uuid)
						}
						switch method {
						case proto.RPCRegisterPath:
							p := params.(map[string]any)
							if p["path"] != modelPath || p["inspect_metadata"] != true {
								t.Fatalf("bad metadata parsing request: %v", p)
							}
							wantStorage := uint(0)
							if entrance == "storage" {
								wantStorage = shared.ID
							}
							if p["storage_id"] != wantStorage {
								t.Fatalf("bad storage scope: %v", p)
							}
							return json.Marshal(registerPathResponse{Path: modelPath, SizeBytes: 123, Metadata: tc.report})
						case proto.RPCFSReadFile:
							if tc.report != nil {
								t.Fatal("unnecessary legacy metadata call")
							}
							return json.RawMessage(`{"missing":true}`), nil
						default:
							t.Fatalf("unexpected RPC %s", method)
							return nil, nil
						}
					}}}
					accepted := tc.localOK
					var failure string
					if entrance == "local" {
						_, previewErr := h.buildLocalModelPreview(context.Background(), node.UUID, modelPath)
						if (previewErr == nil) != accepted {
							t.Fatalf("preview error=%v accepted=%v", previewErr, accepted)
						}
						body, _ := json.Marshal(map[string]string{"node_uuid": node.UUID, "path": modelPath})
						w := httptest.NewRecorder()
						h.registerLocalModel(w, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(string(body))))
						wantCode := http.StatusBadRequest
						if accepted {
							wantCode = http.StatusCreated
						}
						if w.Code != wantCode {
							t.Fatalf("HTTP=%d body=%s", w.Code, w.Body.String())
						}
						failure = w.Body.String()
					} else {
						accepted = tc.storageOK
						_, err := h.importOne(context.Background(), shared, node, root, dto.StorageImportItem{RelPath: "model", Source: "local", SourceID: "model"})
						if (err == nil) != accepted {
							t.Fatalf("import error=%v accepted=%v", err, accepted)
						}
						if err != nil {
							failure = err.Error()
						}
					}
					models, err := st.ListModels()
					if err != nil {
						t.Fatal(err)
					}
					wantCount := 0
					if accepted {
						wantCount = 1
					}
					if len(models) != wantCount {
						t.Fatalf("persisted %d models, want %d", len(models), wantCount)
					}
					if !accepted && !strings.Contains(failure, tc.cause) {
						t.Fatalf("cause %q lost: %s", tc.cause, failure)
					}
					var fileCount int64
					if err := st.DB().Model(&store.ModelFile{}).Count(&fileCount).Error; err != nil {
						t.Fatal(err)
					}
					if !accepted && fileCount != 0 {
						t.Fatalf("rejected import wrote %d file bindings", fileCount)
					}
				})
			}
		})
	}
}
