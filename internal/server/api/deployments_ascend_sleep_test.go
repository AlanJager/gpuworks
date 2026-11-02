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

// A sleepable vLLM deployment on Ascend from a template whose allocator turns
// expandable segments on is not refused: the platform turns the setting off
// and evaluate says so, with the stable warning code and its params.
func TestEvaluateWarnsWhenAscendSleepDisablesTemplateExpandableSegments(t *testing.T) {
	for name, test := range map[string]struct {
		sleepable   bool
		overrides   string
		wantWarning bool
	}{
		"sleepable from the template": {sleepable: true, wantWarning: true},
		"awake keeps the template":    {},
		"the deployment sets its own": {sleepable: true, overrides: `,"container_overrides":{"env":{"PYTORCH_NPU_ALLOC_CONF":"expandable_segments:False"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:ascend_sleep_allocator_"+strings.ReplaceAll(name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			fixture := deploymentFlatFixture()
			fixture.Unsupported = ""
			fixture.Specs[0].Quantization = "bf16"
			fixture.Specs[0].Image = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
			fixture.Specs[0].GPUs = `{"vendor":"ascend"}`
			fixture.Specs[0].Env = `{"PYTORCH_NPU_ALLOC_CONF":"max_split_size_mb:256,expandable_segments:True"}`
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
				t.Fatal(err)
			}
			template, err := st.InferenceModelByModelID(fixture.ModelID)
			if err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "acme/deploy", WeightBytes: 4 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":2,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
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
			body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","model_id":%d,"engine":"vllm","node_uuid":"npu-node","sleepable":%t,"placement":{}%s}`,
				template.ID, model.ID, test.sleepable, test.overrides)
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			var resp struct {
				OK       bool `json:"ok"`
				Warnings []struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"warnings"`
				Errors []dto.FieldError `json:"errors"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || !resp.OK {
				t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
			}
			var found []map[string]any
			for _, warning := range resp.Warnings {
				if warning.Code == "ASCEND_SLEEP_EXPANDABLE_SEGMENTS_DISABLED" {
					found = append(found, warning.Params)
				}
			}
			if !test.wantWarning {
				if len(found) != 0 {
					t.Fatalf("unexpected warning: %+v", found)
				}
				return
			}
			want := map[string]any{
				"variable":       "PYTORCH_NPU_ALLOC_CONF",
				"template_value": "max_split_size_mb:256,expandable_segments:True",
				"value":          "max_split_size_mb:256,expandable_segments:False",
			}
			if len(found) != 1 || !reflect.DeepEqual(found[0], want) {
				t.Fatalf("warnings = %+v, want exactly one with %#v", found, want)
			}
		})
	}
}
