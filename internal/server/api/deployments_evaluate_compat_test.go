package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateRankCompatibilityPreflight(t *testing.T) {
	for name, test := range map[string]struct {
		runtime      string
		overrides    string
		wantWarning  string
		wantV2Script bool
	}{
		"V1 loopback across nodes warns once and still previews every rank": {
			runtime:     "udocker",
			wantWarning: messages.WarningAscendV1CompatMultiNode,
		},
		"final environment selects the multi-node-capable V2 overlay": {
			runtime:      "udocker",
			overrides:    `,"container_overrides":{"env":{"VLLM_USE_V2_MODEL_RUNNER":"1"}}`,
			wantV2Script: true,
		},
		"Docker does not inherit the PRoot restriction": {
			runtime: "docker",
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:api_evaluate_compat_"+strings.ReplaceAll(name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main",
				Name: "Qwen3", Arch: "qwen3", WeightBytes: 1 << 30,
			})
			if err != nil {
				t.Fatal(err)
			}
			for rank, uuid := range []string{"node-1", "node-2"} {
				if _, err := st.RegisterNode(uuid, store.HashToken("t"), uuid); err != nil {
					t.Fatal(err)
				}
				if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
					Sys: proto.SysInfo{Hostname: uuid, GPUVendor: "nvidia", RuntimeDriver: test.runtime}, Network: testManagement(fmt.Sprintf("10.0.0.%d", rank+1)),
					GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"node-1","node_uuids":["node-1","node-2"],"engine":"vllm","image":"quay.io/ascend/vllm-ascend:v0.30.0rc2-a3","nnodes":2,"placement":{"tp":2}%s}`, model.ID, test.overrides)
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
			var resp dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.OK || len(resp.RankCommands) != 2 {
				t.Fatalf("evaluate response = %+v", resp)
			}
			var launchWarnings []messages.Warning
			for _, warning := range resp.Warnings {
				if warning.Code == messages.WarningAscendV1CompatMultiNode {
					launchWarnings = append(launchWarnings, warning)
				}
			}
			if test.wantWarning == "" && len(launchWarnings) != 0 {
				t.Fatalf("unexpected launch warnings: %+v", launchWarnings)
			}
			if test.wantWarning != "" {
				if len(launchWarnings) != 1 {
					t.Fatalf("launch warnings = %+v, want exactly one %s", launchWarnings, test.wantWarning)
				}
				if got := launchWarnings[0].Params["nnodes"]; got != float64(2) && got != 2 {
					t.Fatalf("warning params = %#v", launchWarnings[0].Params)
				}
			}
			for _, rank := range resp.RankCommands {
				hasV2 := strings.Contains(rank.PreStartScript, "# GPUWorks: vLLM-Ascend v0.30.0rc2-a3 V2 compatibility")
				if hasV2 != test.wantV2Script {
					t.Fatalf("rank V2 compatibility = %v, want %v", hasV2, test.wantV2Script)
				}
				if test.runtime == "docker" && rank.PreStartScript != "" {
					t.Fatalf("Docker acquired a PRoot compatibility script")
				}
			}
		})
	}
}
