package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAscendNativeKVOffloadIsRejectedBeforeDeployment(t *testing.T) {
	st, err := store.Open("file:ascend_native_kv_gate?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", Name: "qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	if err := st.UpdateNodeStatus("field-errors-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "field-errors-node", Arch: "arm64", GPUVendor: "ascend", Framework: "cann", FrameworkVersion: "9.1"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, quick := range []string{`{"kv_offloading_size":2}`, `{"kv_disk_offload":true,"shared_kv_dir":"/kv-cache"}`, `{"kv_offloading_size":2,"kv_disk_offload":true,"shared_kv_dir":"/kv-cache","kv_offload_read_threads":4,"kv_offload_write_threads":4,"kv_disk_limit_gib":10}`} {
		code := "KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET"
		if quick == `{"kv_disk_offload":true,"shared_kv_dir":"/kv-cache"}` {
			code = "FIELD_INVALID"
		}
		body := fmt.Sprintf(`{"name":"ascend-kv","model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","image":"gpuworks/gpuworks-runner:cann9.1-910b-vllm0.26.0rc1-r2","quick_params":%s}`, model.ID, quick)
		status, raw := evaluateForFieldErrors(t, h, body)
		if status != http.StatusOK || string(raw["ok"]) != "false" {
			t.Fatalf("evaluate status=%d result=%v", status, raw)
		}
		found := false
		for _, e := range fieldErrorsOf(t, raw) {
			if e.Code == code {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing compatibility error: %s", raw["errors"])
		}
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		createCode := code
		if code == "FIELD_INVALID" {
			createCode = "BAD_REQUEST"
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), createCode) {
			t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}
