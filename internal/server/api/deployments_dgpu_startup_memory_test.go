package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAscendDGPUStartupMemoryAdmission(t *testing.T) {
	const image = "gpuworks/gpuworks-runner:cann9.0-910b-sglang0.5.18-r2"
	const spec = "bf16-sglang-ascend-25218b07"
	for _, tc := range []struct {
		name, placement, quick, extra string
		quota                         int
		refused                       bool
	}{
		{"manual_quick", "manual", `{"gpu_memory_utilization":0.90}`, "", 8, true},
		{"auto_extra", "auto", "{}", "--mem-fraction-static=0.90", 8, true},
		{"manual_safe", "manual", `{"gpu_memory_utilization":0.88}`, "", 8, false},
		{"auto_safe", "auto", `{"gpu_memory_utilization":0.88}`, "", 8, false},
		{"whole_card", "manual", `{"gpu_memory_utilization":0.90}`, "", 0, false},
		{"custom_graph", "manual", `{"gpu_memory_utilization":0.90}`, "--cuda-graph-max-bs 4", 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := partitionPoolFixture(t, map[string]string{"pool-ascend": accelerator.BackendAscendLogical})
			st := h.d.Store
			const repo = "Qwen/Qwen3-0.6B"
			const weightBytes = 1519209873
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: repo, Name: repo, Arch: "qwen3", WeightBytes: weightBytes})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
				ModelID: strings.ToLower(repo), DisplayTitle: repo, Source: "upstream", Architecture: "qwen3",
				Specs: []store.InferenceSpecRow{{
					Name: spec, Engine: "sglang", Image: image, Quantization: "bf16", WeightBytes: weightBytes,
					WeightSources: `[{"provider":"huggingface","repo_id":"Qwen/Qwen3-0.6B"}]`,
					GPUs:          `{"vendor":"ascend"}`,
					Args:          `["--device","npu","--attention-backend","ascend","--dtype","bfloat16"]`,
				}},
			}}); err != nil {
				t.Fatal(err)
			}
			template, err := st.InferenceModelByModelID(strings.ToLower(repo))
			if err != nil {
				t.Fatal(err)
			}
			node := ""
			if tc.placement == "manual" {
				node = "pool-ascend"
			}
			body := fmt.Sprintf(`{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":%q,"engine":"sglang","image":%q,"node_uuid":%q,"placement_mode":%q,"target_vendor":"ascend","placement":{"tp":1},"vram_limit_gib":%d,"quick_params":%s,"extra_args":%q}`,
				tc.name, model.ID, template.ID, spec, image, node, tc.placement, tc.quota, tc.quick, tc.extra)

			wire := evaluateWire(t, h, body)
			var fields []dto.FieldError
			if raw := wire["errors"]; raw != nil {
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
			}
			if tc.refused {
				if string(wire["ok"]) != "false" || len(fields) != 1 || fields[0].Code != messages.ErrorDGPUStartupMemory ||
					fields[0].Field != "gpu_memory_utilization" || fields[0].Params["max_utilization"] != 0.88 {
					t.Fatalf("evaluate = %s, errors = %+v; want the startup memory error on the utilization field", wire["ok"], fields)
				}
				requireCreateRefusalWithoutDeployment(t, h, st, body, messages.ErrorDGPUStartupMemory)
				return
			}
			if string(wire["ok"]) != "true" || len(fields) != 0 {
				t.Fatalf("evaluate = %s, errors = %+v; want accepted", wire["ok"], fields)
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create = %d %s, want accepted", created.Code, created.Body)
			}
		})
	}
}
