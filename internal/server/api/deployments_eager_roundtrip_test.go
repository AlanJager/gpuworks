package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRouterEagerTemplateFeedbackAndCreate(t *testing.T) {
	_, st := automaticPlacementFixture(t)
	automaticPlacementModel(t, st)
	router, token := kunlunRouter(t, st)
	var specs []store.InferenceSpecRow
	for _, engine := range []string{"vllm", "sglang"} {
		for _, templateOn := range []bool{false, true} {
			args := `[]`
			if templateOn {
				if engine == "sglang" {
					args = `["--disable-cuda-graph"]`
				} else {
					args = `["--enforce-eager"]`
				}
			}
			specs = append(specs, store.InferenceSpecRow{Name: fmt.Sprintf("%s-%v", engine, templateOn), Scenario: "balanced", Quantization: "bf16", Engine: engine, Image: "example/engine:test", WeightBytes: 1 << 30, WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Eager"}]`, GPUs: `{"vendor":"nvidia"}`, Args: args})
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: "test/eager", DisplayTitle: "Eager", Source: "upstream", Architecture: "qwen3", ContextLength: 4096, Specs: specs}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("test/eager")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		for _, value := range []any{nil, false, true} {
			t.Run(fmt.Sprintf("%s/%v", spec.Name, value), func(t *testing.T) {
				request := map[string]any{"name": fmt.Sprintf("eager-%s-%v", spec.Name, value), "served_name": fmt.Sprintf("eager-%s-%v", spec.Name, value), "template_id": template.ID, "template_spec_name": spec.Name, "node_uuid": "auto-big", "priority_scheduling": false}
				if value != nil {
					request["quick_params"] = map[string]any{"enforce_eager": value}
				}
				flag := "--enforce-eager"
				if spec.Engine == "sglang" {
					flag = "--disable-cuda-graph"
				}
				want := value
				if value == nil && spec.Args != `[]` {
					want = true
				}
				for attempt := 0; attempt < 2; attempt++ {
					raw, _ := json.Marshal(request)
					status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
					var preview dto.EvaluateResponse
					if status != http.StatusOK || json.Unmarshal([]byte(body), &preview) != nil || !preview.OK {
						t.Fatalf("evaluate %d %s", status, body)
					}
					if _, present := preview.EffectiveParams["enforce_eager"]; present != (want != nil) {
						t.Fatalf("effective eager presence=%v want=%v", present, want)
					}
					if preview.EffectiveParams["enforce_eager"] != want || slices.Contains(append(preview.Entrypoint, preview.Argv...), flag) != (want == true) {
						t.Fatalf("value=%v preview=%s", value, body)
					}
					request["quick_params"] = preview.FormValues
					request["extra_args"] = preview.FormValues["extra_args"]
					delete(preview.FormValues, "extra_args")
				}
				raw, _ := json.Marshal(request)
				status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/deployments", string(raw))
				var dep dto.Deployment
				if status != http.StatusCreated || json.Unmarshal([]byte(body), &dep) != nil {
					t.Fatalf("create %d %s", status, body)
				}
				status, body = callRouter(t, router, token, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", dep.ID), "")
				var draft dto.DeploymentDraft
				if status != http.StatusOK || json.Unmarshal([]byte(body), &draft) != nil || draft.Deployment.QuickParams["enforce_eager"] != want {
					t.Fatalf("draft %d %s", status, body)
				}
				if _, present := draft.Deployment.QuickParams["enforce_eager"]; present != (want != nil) {
					t.Fatalf("stored eager presence=%v want=%v", present, want)
				}
				feedback, _ := json.Marshal(dto.EvaluateRequest{DeployRequest: draft.Deployment, DeploymentID: dep.ID})
				status, body = callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(feedback))
				var preview dto.EvaluateResponse
				if status != http.StatusOK || json.Unmarshal([]byte(body), &preview) != nil || !preview.OK || preview.EffectiveParams["enforce_eager"] != want || slices.Contains(append(preview.Entrypoint, preview.Argv...), flag) != (want == true) {
					t.Fatalf("stored evaluate %d %s", status, body)
				}
				if _, present := preview.EffectiveParams["enforce_eager"]; present != (want != nil) {
					t.Fatalf("stored preview eager presence=%v want=%v", present, want)
				}
			})
		}
	}
}
