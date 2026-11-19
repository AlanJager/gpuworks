package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestRouterPrioritySchedulingEffectiveLaunch(t *testing.T) {
	_, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	router, token := kunlunRouter(t, st)
	for i, tc := range []struct {
		name, engine, entry, args, mode string
		allowed                         bool
	}{
		{"sglang default LLM", "sglang", "", "", "", true},
		{"sglang explicit LLM", "sglang", "sglang serve", "--model-type llm", "", true},
		{"sglang diffusion", "sglang", "sglang serve --model-type diffusion", "", "", false},
		{"sglang detected", "sglang", "sglang serve", "", "", false},
		{"sglang vllm entry", "sglang", "vllm serve", "", "", false},
		{"sglang custom entry", "sglang", "custom-server", "", "", false},
		{"vllm default LLM", "vllm", "", "", "", true},
		{"vllm API server", "vllm", "python3 -m vllm.entrypoints.openai.api_server", "", "", true},
		{"vllm omni", "vllm", "vllm serve", "--omni", "", false},
		{"wan pipeline", "vllm", "vllm-omni", "--model-type wan2_2_ti2v", "", false},
		{"vllm custom entry", "vllm", "custom-server", "", "", false},
		{"unknown image entry", "vllm", "", "", "image-entry", false},
		{"manual", "vllm", "custom-server", "", "manual", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, priority := range []any{nil, false, true} {
				request := map[string]any{"name": fmt.Sprintf("launch-%d-%v", i, priority), "served_name": fmt.Sprintf("launch-%d-%v", i, priority), "model_id": modelID, "engine": tc.engine, "image": "example/engine:test", "node_uuid": "auto-big", "extra_args": tc.args, "quick_params": map[string]any{}}
				// The API command editor uses display strings, not runtime argv arrays.
				request["container_overrides"] = map[string]any{"entrypoint_display": tc.entry, "command_mode": tc.mode}
				if priority != nil {
					request["priority_scheduling"] = priority
				}
				raw, _ := json.Marshal(request)
				status, body := callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
				var preview dto.EvaluateResponse
				if status != http.StatusOK || json.Unmarshal([]byte(body), &preview) != nil {
					t.Fatalf("evaluate %d %s", status, body)
				}
				if preview.PriorityScheduling == nil || preview.PriorityScheduling.OK != tc.allowed {
					t.Fatalf("eligibility allowed=%v: %s", tc.allowed, body)
				}
				if !tc.allowed {
					if preview.PriorityScheduling.Refusal == nil || preview.PriorityScheduling.Refusal.Code != "PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED" || len(preview.PriorityScheduling.Refusal.Params) != 1 || preview.PriorityScheduling.Refusal.Params["engine"] != tc.engine {
						t.Fatalf("imprecise refusal: %+v", preview.PriorityScheduling)
					}
					if priority == true && (preview.OK || len(preview.Errors) != 1 || preview.Errors[0].Field != "priority_scheduling" || preview.Errors[0].Code != "PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED") {
						t.Fatalf("explicit unsupported evaluate: %s", body)
					}
				}
				wantPriority := tc.allowed && priority != false
				hasPriority := slices.Contains(append(preview.Entrypoint, preview.Argv...), "--enable-priority-scheduling") || slices.Contains(append(preview.Entrypoint, preview.Argv...), "--scheduling-policy")
				if hasPriority != wantPriority {
					t.Fatalf("priority=%v argv=%v", priority, preview.Argv)
				}
				status, body = callRouter(t, router, token, http.MethodPost, "/api/v1/deployments", string(raw))
				if priority == true && !tc.allowed {
					if status != http.StatusBadRequest || !strings.Contains(body, "PRIORITY_SCHEDULING_LAUNCH_UNSUPPORTED") || !strings.Contains(body, `"field":"priority_scheduling"`) {
						t.Fatalf("explicit unsupported create %d %s", status, body)
					}
					return
				}
				var dep dto.Deployment
				if status != http.StatusCreated || json.Unmarshal([]byte(body), &dep) != nil {
					t.Fatalf("create %d %s", status, body)
				}
				if dep.PriorityScheduling != wantPriority {
					t.Fatalf("create priority=%v want=%v", dep.PriorityScheduling, wantPriority)
				}
				status, body = callRouter(t, router, token, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d/draft", dep.ID), "")
				var draft dto.DeploymentDraft
				if status != http.StatusOK || json.Unmarshal([]byte(body), &draft) != nil {
					t.Fatalf("draft %d %s", status, body)
				}
				raw, _ = json.Marshal(dto.EvaluateRequest{DeployRequest: draft.Deployment, DeploymentID: dep.ID})
				status, body = callRouter(t, router, token, http.MethodPost, "/api/v1/evaluate", string(raw))
				if status != http.StatusOK || json.Unmarshal([]byte(body), &preview) != nil || !preview.OK {
					t.Fatalf("stored evaluate %d %s", status, body)
				}
				if slices.Contains(append(preview.Entrypoint, preview.Argv...), "--enable-priority-scheduling") != (wantPriority && tc.engine == "sglang") || slices.Contains(append(preview.Entrypoint, preview.Argv...), "--scheduling-policy") != (wantPriority && tc.engine == "vllm") {
					t.Fatalf("stored launch disagrees: %s", body)
				}

			}
		})
	}
}
