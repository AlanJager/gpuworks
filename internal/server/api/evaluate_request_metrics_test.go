package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// The deploy preview shows vLLM's per-request metrics flag exactly when the
// launch would carry it: for an image known to run vLLM 0.25.0 or later,
// whether a node was picked or none is eligible (the preview then falls back
// to a node-less command). Everywhere else the switch is left to the engine
// and starts at automatic, so the form never shows a position the command
// does not carry.
func TestEvaluatePreviewGatesPerRequestMetricsOnTheImageVersion(t *testing.T) {
	h, templateID := evaluateFixtureWorld(t)
	var modelID uint
	models, _ := h.d.Store.ListModels()
	for _, model := range models {
		if model.SourceID == "Acme/Deploy-FP8" {
			modelID = model.ID
		}
	}
	cases := []struct {
		name, body string
		want       bool
	}{
		{"node, versioned image", fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"vllm/vllm-openai:v0.30.0"}`, modelID), true},
		{"node, image of unknown version", fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"private.example/vllm:custom"}`, modelID), false},
		{"node, release before the flag", fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"vllm/vllm-openai:v0.24.0"}`, modelID), false},
		{"no eligible node, versioned image", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"future-hardware","engine":"vllm","placement_mode":"auto","image":"vllm/vllm-openai:v0.30.0"}`, modelID, templateID), true},
		{"no eligible node, unknown version", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"future-hardware","engine":"vllm","placement_mode":"auto"}`, modelID, templateID), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(c.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if got := strings.Contains(rec.Body.String(), `"--enable-per-request-metrics"`); got != c.want {
				t.Fatalf("flag in preview = %v, want %v: %s", got, c.want, rec.Body.String())
			}
			var response dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			engineDecides := slices.Contains(response.EngineDecidesParamKeys, "enable_request_metrics")
			_, stated := response.FormValues["enable_request_metrics"]
			if engineDecides == c.want || stated != c.want {
				t.Fatalf("left to the engine = %v, stated = %v, want default on = %v: %v", engineDecides, stated, c.want, response.EngineDecidesParamKeys)
			}
		})
	}
}
