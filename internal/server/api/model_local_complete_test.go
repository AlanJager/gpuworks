package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const offlineCommit = "25d1d1fa49de445ab1f3a41f3537d7dad222826e"

func registerPathRig(t *testing.T, name string, answer func(params map[string]any) string) (*apiHandlers, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("node-1", store.HashToken("t"), "node"); err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st, CallNode: func(_ context.Context, _, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCRegisterPath {
			return nil, fmt.Errorf("unexpected rpc %s", method)
		}
		raw, _ := json.Marshal(params)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return json.RawMessage(answer(decoded)), nil
	}}}, st
}

func registerPath(t *testing.T, h *apiHandlers, id uint) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/models/%d/register-path", id), strings.NewReader(`{"node_uuid":"node-1","path":"/models/Qwen3"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(id)}))
	rec := httptest.NewRecorder()
	h.registerModelPath(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// A Hugging Face model registered while offline has no metadata; registering
// the directory copied onto a node fills it from the directory, and pins the
// commit the huggingface download left there.
func TestRegisterPathCompletesAnOfflineModelFromItsCopy(t *testing.T) {
	var asked map[string]any
	h, st := registerPathRig(t, "api_register_path_complete", func(params map[string]any) string {
		asked = params
		return `{"path":"/models/Qwen3","size_bytes":4096,"revision":"` + offlineCommit + `","metadata":{"entries":[{"path":".","format":"transformers"}],"config_raw":"{\"architectures\":[\"Qwen3ForCausalLM\"],\"model_type\":\"qwen3\",\"torch_dtype\":\"bfloat16\",\"max_position_embeddings\":40960}","readme":"# Qwen3"}}`
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main", Name: "Qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	registerPath(t, h, model.ID)
	if asked["inspect_metadata"] != true {
		t.Fatalf("the node was not asked for the directory's metadata: %v", asked)
	}
	got, err := st.GetModel(model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Arch != "Qwen3ForCausalLM" || got.WeightBytes != 4096 || got.ContextLength != 40960 || got.README != "# Qwen3" || got.Meta == "" || got.Precision != "bfloat16" {
		t.Fatalf("model after register-path = %+v", got)
	}
	if got.Revision != offlineCommit {
		t.Fatalf("revision = %q, want the download's commit", got.Revision)
	}
}

// A model that has its metadata keeps it: the directory fills only what is
// missing. The node still validates the selected directory.
func TestRegisterPathKeepsTheMetadataAModelHas(t *testing.T) {
	var asked map[string]any
	h, st := registerPathRig(t, "api_register_path_keep", func(params map[string]any) string {
		asked = params
		return `{"path":"/models/Qwen3","size_bytes":4096,"metadata":{"entries":[{"path":".","format":"transformers"}]}}`
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: offlineCommit, Name: "Qwen3", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3"}`, WeightBytes: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	registerPath(t, h, model.ID)
	if asked["inspect_metadata"] != true {
		t.Fatalf("a model with metadata bypassed directory validation: %v", asked)
	}
	got, _ := st.GetModel(model.ID)
	if got.WeightBytes != 8<<30 || got.Revision != offlineCommit {
		t.Fatalf("model after register-path = %+v", got)
	}
}

// A known model's metadata must not let an arbitrary node directory bypass
// validation or create a registered file row.
func TestRegisterPathRejectsInvalidDirectoryMetadataBeforePersisting(t *testing.T) {
	for _, report := range []string{`null`, `{}`, `{"component":true}`, `{"entries":[{"path":".","incomplete":true,"missing":["missing.safetensors"]}]}`, `{"entries":[{"path":"first"},{"path":"second"}]}`} {
		t.Run(report, func(t *testing.T) {
			h, st := registerPathRig(t, "api_register_invalid_"+fmt.Sprint(len(report)), func(params map[string]any) string {
				if params["inspect_metadata"] != true {
					t.Fatal("known model skipped directory inspection")
				}
				return `{"path":"/models/parent","size_bytes":999,"metadata":` + report + `}`
			})
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: offlineCommit, Name: "Qwen3", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3"}`, WeightBytes: 100})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/register-path", strings.NewReader(`{"node_uuid":"node-1","path":"/models/parent"}`))
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
			rec := httptest.NewRecorder()
			h.registerModelPath(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			files, err := st.ModelFilesForModel(model.ID)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid registration persisted files=%+v err=%v", files, err)
			}
		})
	}
}
