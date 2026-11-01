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
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A sleepable vLLM deployment on Ascend refuses a nonzero weight_nz_mode the
// user typed, with the stable code and its params on the wire, at evaluate
// (beside extra_args) and at create (400).
func TestEvaluateAndCreateRefuseNonzeroWeightNZForAscendSleep(t *testing.T) {
	st, err := store.Open("file:ascend_sleep_weight_nz?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("npu-node", store.HashToken("token"), "npu-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("npu-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "npu-node", Arch: "arm64", GPUVendor: "ascend", Framework: "cann", FrameworkVersion: "8.3"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "Ascend 910C", Vendor: "ascend", ArchID: "910C", MemoryTotalBytes: 64 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: st}}
	request := func(extra string) string {
		encoded, err := json.Marshal(extra)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`{"name":"npu-sleep","served_name":"npu-sleep","model_id":%d,"node_uuid":"npu-node","engine":"vllm","image":"quay.io/ascend/vllm-ascend:v0.30.0rc2-a3","sleepable":true,"quick_params":{},"extra_args":%s}`, model.ID, encoded)
	}
	bad := request(`--additional-config '{"weight_nz_mode":2,"other":true}'`)

	evaluated := httptest.NewRecorder()
	handler.evaluate(evaluated, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(bad)))
	var answer struct {
		OK     bool             `json:"ok"`
		Errors []dto.FieldError `json:"errors"`
	}
	if evaluated.Code != http.StatusOK || json.Unmarshal(evaluated.Body.Bytes(), &answer) != nil || answer.OK {
		t.Fatalf("evaluate=%d: %s", evaluated.Code, evaluated.Body.String())
	}
	found := false
	for _, item := range answer.Errors {
		if item.Code == ErrorAscendSleepWeightNZConflict {
			found = true
			if item.Field != "extra_args" || item.Params["flag"] != "--additional-config" || item.Params["value"] != "2" || len(item.Params) != 2 {
				t.Fatalf("evaluate error = %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("evaluate errors = %+v, want %s", answer.Errors, ErrorAscendSleepWeightNZConflict)
	}

	created := httptest.NewRecorder()
	handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(bad)))
	var refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if created.Code != http.StatusBadRequest || json.Unmarshal(created.Body.Bytes(), &refusal) != nil ||
		refusal.Code != ErrorAscendSleepWeightNZConflict || refusal.Params["flag"] != "--additional-config" || refusal.Params["value"] != "2" {
		t.Fatalf("create=%d, want 400 %s: %s", created.Code, ErrorAscendSleepWeightNZConflict, created.Body.String())
	}

	good := httptest.NewRecorder()
	handler.createDeployment(good, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(request(`--additional-config '{"other":true}'`))))
	if good.Code != http.StatusCreated {
		t.Fatalf("create without weight_nz_mode=%d: %s", good.Code, good.Body.String())
	}
}
