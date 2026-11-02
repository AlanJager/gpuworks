package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPoolingUsesResolvedExistingModelContract(t *testing.T) {
	for _, tc := range []struct {
		name, operations string
		statedModel      bool
	}{
		{"template_chat", `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`, false},
		{"explicit_chat", `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`, true},
		{"template_mixed", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"},{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, h, id := templateOperationsFixture(t, "pooling_resolved_"+tc.name)
			model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed", WeightBytes: 1 << 30, Operations: tc.operations})
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"name": "review pool", "template_id": id, "template_spec_name": "embed", "node_uuid": "pooling_resolved_" + tc.name + "-node", "image": "vllm/vllm-openai:v0.23.0"}
			if tc.statedModel {
				body["model_id"] = model.ID
			}
			raw, _ := json.Marshal(body)
			response := httptest.NewRecorder()
			h.evaluate(response, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(raw))))
			if response.Code != 200 {
				t.Fatalf("evaluate=%d %s", response.Code, response.Body.String())
			}
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if !preview.OK {
				t.Fatalf("preview not OK %s", response.Body.String())
			}
			created := createTemplateDeployment(t, h, body)
			stored, err := st.GetDeployment(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot deploy.CatalogSelection
			if err := json.Unmarshal([]byte(stored.CatalogSelectionSnapshot), &snapshot); err != nil {
				t.Fatal(err)
			}
			_, previewPooling := flagValue(append(preview.Entrypoint, preview.Argv...), "--runner")
			value, createdPooling := flagValue(snapshot.ExtraArgs, "--runner")
			if previewPooling || createdPooling {
				t.Fatalf("same request preview runner=%v, persisted runner=%v value=%q; actual deployment operations=%v", previewPooling, createdPooling, value, deploymentOperationTypes(created))
			}
		})
	}
}
