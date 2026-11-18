package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAutomaticPlacementPermanentRefusalAgreesWithCreate(t *testing.T) {
	for _, tc := range []struct {
		name, pool, placement string
		wantPool, wantShapes  int
	}{
		{"too_few_cards", `{"gpuworks.ai/node":["auto-big"]}`, `{"tp":4}`, 1, 1},
		{"too_little_memory", `{"gpuworks.ai/node":["auto-small"]}`, `{}`, 1, 1},
		{"empty_pool", `{"pool":["missing"]}`, `{}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st := automaticPlacementFixture(t)
			modelID := automaticPlacementModel(t, st)
			if err := st.UpdateNodeStatus("auto-big", proto.NodeStatus{
				Sys: proto.SysInfo{Hostname: "auto-big", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
				GPUs: []proto.GPUInfo{
					{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true},
					{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true},
				},
			}); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"name":%q,"model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_requirements":%s,"placement":%s,"quick_params":{}}`, tc.name, modelID, tc.pool, tc.placement)
			evaluated := httptest.NewRecorder()
			h.evaluate(evaluated, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(evaluated.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if evaluated.Code != http.StatusOK || preview.OK || preview.SelectedNode != nil || len(preview.Errors) != 1 || preview.Errors[0].Code != ErrorDeploymentNeverFits {
				t.Fatalf("evaluate = %d %s, want a blocking permanent-capacity error", evaluated.Code, evaluated.Body.String())
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			var refusal struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &refusal); err != nil {
				t.Fatal(err)
			}
			if created.Code != http.StatusBadRequest || refusal.Code != ErrorDeploymentNeverFits || !reflect.DeepEqual(refusal.Params, preview.Errors[0].Params) || refusal.Params["pool"] != float64(tc.wantPool) || refusal.Params["shapes"] != float64(tc.wantShapes) {
				t.Fatalf("create = %d %s, want the same permanent refusal as evaluate", created.Code, created.Body.String())
			}
			if rows, err := st.ListDeployments(); err != nil || len(rows) != 0 {
				t.Fatalf("refused request persisted deployments: %v %v", rows, err)
			}
		})
	}
}

func TestAutomaticPlacementTemporaryUnavailabilityRemainsRetryable(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline_%v", offline), func(t *testing.T) {
			h, st := automaticPlacementFixture(t)
			modelID := automaticPlacementModel(t, st)
			h.d.Online = func(string) bool { return !offline }
			if offline {
				if err := st.SetNodeState("auto-big", store.NodeStateOffline); err != nil {
					t.Fatal(err)
				}
			}
			if !offline {
				node, err := st.NodeByUUID("auto-big")
				if err != nil {
					t.Fatal(err)
				}
				dep, err := st.CreateDeployment(&store.Deployment{Name: "occupant", ModelID: modelID, NodeID: node.ID, Engine: "vllm"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.CreateInstance(&store.Instance{UUID: "occupied", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"name":"wait","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_requirements":{"gpuworks.ai/node":["auto-small","auto-big"]},"quick_params":{}}`, modelID)
			evaluated := httptest.NewRecorder()
			h.evaluate(evaluated, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(evaluated.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			if evaluated.Code != http.StatusOK || !preview.OK || len(preview.Errors) != 0 || preview.SelectedNode != nil {
				t.Fatalf("temporary unavailability blocked the draft: %d %s", evaluated.Code, evaluated.Body.String())
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusConflict || !strings.Contains(created.Body.String(), ErrorAutomaticPlacementUnavailable) {
				t.Fatalf("temporary create = %d %s, want retryable conflict", created.Code, created.Body.String())
			}
		})
	}
}
