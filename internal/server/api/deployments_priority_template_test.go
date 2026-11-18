package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRouterPrioritySchedulingTemplateAndEdit(t *testing.T) {
	_, st := automaticPlacementFixture(t)
	router, token := kunlunRouter(t, st)
	spec := func(name, engine, entry, args string) store.InferenceSpecRow {
		return store.InferenceSpecRow{Name: name, Scenario: "balanced", Quantization: "bf16", Engine: engine, Image: "example/engine:test", WeightBytes: 1 << 30, WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Priority"}]`, GPUs: `{"vendor":"nvidia"}`, Entrypoint: entry, Args: args}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: "test/priority", DisplayTitle: "Priority", Source: "upstream", Architecture: "qwen3", ContextLength: 4096, Specs: []store.InferenceSpecRow{
		spec("diffusion", "sglang", `["sglang","serve","--model-type","diffusion"]`, `[]`),
		spec("llm", "sglang", `["sglang","serve"]`, `["--model-type","llm"]`),
		spec("pipeline", "sglang", `["sglang","serve"]`, `["--model-type","diffusion","--model-path","wan2_2_ti2v"]`),
		spec("omni", "vllm", `["vllm","serve"]`, `["--omni"]`),
	}}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("test/priority")
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, spec, args, entry string
		allowed                 bool
	}{
		{"template diffusion", "diffusion", "", "", false},
		{"user model type wins", "diffusion", "--model-type llm", "", true},
		{"user disables LLM", "llm", "--model-type=diffusion", "", false},
		{"user replaces entrypoint", "diffusion", "", "python3 -m sglang.launch_server", true},
		{"user mismatched entrypoint", "llm", "", "vllm serve", false},
		{"pipeline owner", "pipeline", "", "", false},
		{"omni template", "omni", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"name": fmt.Sprintf("template-%d", i), "served_name": fmt.Sprintf("template-%d", i), "template_id": template.ID, "template_spec_name": tc.spec, "node_uuid": "auto-big", "extra_args": tc.args}
			if tc.entry != "" {
				request["container_overrides"] = map[string]any{"entrypoint_display": tc.entry}
			}
			raw, _ := json.Marshal(request)
			status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
			var preview dto.EvaluateResponse
			if status != http.StatusOK || json.Unmarshal([]byte(body), &preview) != nil || !preview.OK || preview.PriorityScheduling == nil || preview.PriorityScheduling.OK != tc.allowed {
				t.Fatalf("template evaluate %d %s", status, body)
			}
			command := append(preview.Entrypoint, preview.Argv...)
			if (slices.Contains(command, "--enable-priority-scheduling") || slices.Contains(command, "--scheduling-policy")) != tc.allowed {
				t.Fatalf("template command=%v", command)
			}
			status, body = callRouter(t, router, token, http.MethodPost, "/api/v1/deployments", string(raw))
			var dep dto.Deployment
			if status != http.StatusCreated || json.Unmarshal([]byte(body), &dep) != nil || dep.PriorityScheduling != tc.allowed {
				t.Fatalf("template create %d %s", status, body)
			}
			status, body = callRouter(t, router, token, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", dep.ID), "")
			var draft dto.DeploymentDraft
			if status != http.StatusOK || json.Unmarshal([]byte(body), &draft) != nil {
				t.Fatalf("draft %d %s", status, body)
			}
			// An edit must enforce the same current executable contract as evaluate/create.
			enabled := true
			draft.Deployment.PriorityScheduling = &enabled
			edit, _ := json.Marshal(dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision})
			status, body = callRouter(t, router, token, http.MethodPut, fmt.Sprintf("/api/v1/deployments/%d?dry_run=1", dep.ID), string(edit))
			if tc.allowed {
				if status != http.StatusOK {
					t.Fatalf("allowed edit %d %s", status, body)
				}
			} else if status != http.StatusBadRequest || !strings.Contains(body, "PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED") || !strings.Contains(body, `"field":"priority_scheduling"`) {
				t.Fatalf("unsupported edit %d %s", status, body)
			}
		})
	}
}
