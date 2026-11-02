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
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestLocalRegistrationPreservesUnrecognizedCheckpointCause(t *testing.T) {
	const index = "quant_model_weights.safetensors.index.json"
	dir := t.TempDir()
	for name, body := range map[string]string{
		"config.json":       `{"architectures":["ExampleForCausalLM"]}`,
		index:               `{"weight_map":`,
		"model.safetensors": "unrelated weights cannot replace the invalid index",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("checkpoint-regression", store.HashToken("fixture-token"), "checkpoint-regression")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if uuid != node.UUID || method != proto.RPCRegisterPath {
			t.Fatalf("unexpected RPC: node=%s method=%s", uuid, method)
		}
		p := params.(map[string]any)
		if p["path"] != dir || p["inspect_metadata"] != true {
			t.Fatalf("unexpected metadata request: %v", p)
		}
		report, err := modelscan.ParseMetadata(ctx, dir)
		if err != nil {
			return nil, err
		}
		// Older agents return the report without enforcing registration admission.
		return json.Marshal(registerPathResponse{Path: dir, Metadata: &report})
	}}}
	body, err := json.Marshal(map[string]string{"node_uuid": node.UUID, "path": dir})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.registerLocalModel(w, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(string(body))))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), index) || !strings.Contains(w.Body.String(), "EOF") {
		t.Fatalf("checkpoint cause lost: HTTP=%d body=%s", w.Code, w.Body.String())
	}
	models, err := st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("rejected checkpoint wrote %d models", len(models))
	}
	var fileCount int64
	if err := st.DB().Model(&store.ModelFile{}).Count(&fileCount).Error; err != nil {
		t.Fatal(err)
	}
	if fileCount != 0 {
		t.Fatalf("rejected checkpoint wrote %d file bindings", fileCount)
	}
}
