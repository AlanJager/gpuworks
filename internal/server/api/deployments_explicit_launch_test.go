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
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestQuickEagerDictionaryPayloadResolvesEngineFlag(t *testing.T) {
	st, err := store.Open("file:quick_eager_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/eager", Name: "eager", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("eager-node", store.HashToken("token"), "eager-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("eager-node", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "eager-node", GPUVendor: "nvidia", Framework: "cuda"}, GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	for _, tc := range []struct {
		engine, image, flag string
	}{
		{engine: "vllm", image: "vllm/vllm-openai:latest", flag: "--enforce-eager"},
		{engine: "sglang", image: "lmsysorg/sglang:latest", flag: "--disable-cuda-graph"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			profile, err := deploy.ProfileForEngine(tc.engine)
			if err != nil {
				t.Fatal(err)
			}
			shown := false
			for _, key := range profile.QuickFormKeys {
				shown = shown || key == "enforce_eager"
			}
			if !shown {
				t.Fatalf("%s quick dictionary hides enforce_eager", tc.engine)
			}
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"eager-node","engine":%q,"image":%q,"served_name":%q,"quick_params":{"enforce_eager":true}}`, model.ID, tc.engine, tc.image, tc.engine)
			preview := httptest.NewRecorder()
			h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if preview.Code != http.StatusOK {
				t.Fatalf("evaluate=%d %s", preview.Code, preview.Body.String())
			}
			var resolved dto.EvaluateResponse
			if err := json.Unmarshal(preview.Body.Bytes(), &resolved); err != nil {
				t.Fatal(err)
			}
			if got, ok := resolved.EffectiveParams["enforce_eager"].(bool); !ok || !got || !strings.Contains(" "+strings.Join(resolved.Command, " ")+" ", " "+tc.flag+" ") {
				t.Fatalf("quick eager did not reach %s launch: params=%v command=%v", tc.engine, resolved.EffectiveParams, resolved.Command)
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d %s", created.Code, created.Body.String())
			}
		})
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) != 2 {
		t.Fatalf("deployments=%+v err=%v", deployments, err)
	}
	for _, d := range deployments {
		if !strings.Contains(d.QuickParams, `"enforce_eager":true`) {
			t.Fatalf("%s create lost eager choice: %s", d.Engine, d.QuickParams)
		}
	}
}

func TestCustomImagePreviewAndCreateKeepExplicitLaunchContract(t *testing.T) {
	st, err := store.Open("file:explicit_custom_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/custom", Revision: "main", Name: "custom", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("custom-node", store.HashToken("token"), "custom-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("custom-node", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "custom-node", GPUVendor: "nvidia", Framework: "cuda"}, GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":"custom-node","engine":"sglang","image":"private.example/sglang:custom","replicas":3,"extra_args":"--log-level error","container_overrides":{"env":{"USER_FLAG":"yes"},"extra_mounts":[{"container_path":"/work","host_path":"/work"}]}}`, model.ID)
	h := &apiHandlers{d: Deps{Store: st}}
	preview := httptest.NewRecorder()
	h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if preview.Code != http.StatusOK {
		t.Fatalf("evaluate=%d %s", preview.Code, preview.Body.String())
	}
	var evaluated dto.EvaluateResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &evaluated); err != nil {
		t.Fatal(err)
	}
	if evaluated.CommandMode != "engine" || evaluated.Replicas != 3 || evaluated.PlanScope != "per_replica" || evaluated.Env["USER_FLAG"] != "yes" || evaluated.Env["SGLANG_LOGGING_CONFIG_PATH"] == "" || evaluated.PreStartScript == "" || len(evaluated.Mounts) != 1 {
		t.Fatalf("preview lost explicit launch inputs: %+v", evaluated)
	}
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments=%+v err=%v", deployments, err)
	}
	// The snapshot records the template selection and acquisition only; there
	// is no launch-contract marker to interpret (ADR 0031).
	if strings.Contains(deployments[0].CatalogSelectionSnapshot, "launch_contract") {
		t.Fatalf("snapshot carries a retired marker: %s", deployments[0].CatalogSelectionSnapshot)
	}
	if deployments[0].Image != "private.example/sglang:custom" || deployments[0].Replicas != 3 || !strings.Contains(deployments[0].QuickParams, `"log_level":"error"`) {
		t.Fatalf("stored inputs diverged: %+v", deployments[0])
	}
}

func TestEvaluateAndCreateShareReplicaValidation(t *testing.T) {
	st, err := store.Open("file:replica_shape_contract?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st}}
	for _, tc := range []struct {
		name, shape, code string
	}{
		{name: "negative", shape: `"replicas":-1`, code: ErrorReplicasNegative},
		{name: "over limit", shape: `"replicas":33`, code: ErrorReplicasTooMany},
		{name: "distributed replicas", shape: `"replicas":2,"nnodes":2`, code: ErrorDistributedReplicaCount},
	} {
		for _, endpoint := range []string{"/api/v1/evaluate", "/api/v1/deployments"} {
			t.Run(tc.name+endpoint, func(t *testing.T) {
				body := `{` + tc.shape + `}`
				rec := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
				if endpoint == "/api/v1/evaluate" {
					// Evaluation is the interactive endpoint: the same code
					// create answers 400 with arrives as a field error on a
					// 200/ok=false response, so the form keeps the draft.
					h.evaluate(rec, request)
					if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.code) {
						t.Fatalf("status/body=%d %s", rec.Code, rec.Body.String())
					}
					var evaluated struct {
						OK     bool             `json:"ok"`
						Errors []dto.FieldError `json:"errors"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
						t.Fatal(err)
					}
					if evaluated.OK || len(evaluated.Errors) != 1 || evaluated.Errors[0].Field != "replicas" || evaluated.Errors[0].Code != tc.code {
						t.Fatalf("evaluate body=%s", rec.Body.String())
					}
					return
				}
				h.createDeployment(rec, request)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.code) {
					t.Fatalf("status/body=%d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
	models, _ := st.ListModels()
	deployments, _ := st.ListDeployments()
	if len(models) != 0 || len(deployments) != 0 {
		t.Fatalf("invalid replica shapes caused side effects: models=%v deployments=%v", models, deployments)
	}
}

func TestDirectCreateRejectsInvalidLaunchBeforeModelRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, overrides string
		// quick is extra quick_params JSON; code is the product error code
		// the 400 must carry when the case is about one.
		quick, code string
	}{
		{name: "model mount conflict", overrides: `{"extra_mounts":[{"container_path":"/model","host_path":"/elsewhere"}]}`},
		{name: "bad entrypoint", overrides: `{"entrypoint_display":"[bad"}`},
		{name: "bad environment", overrides: `{"env":{"KEY":23}}`},
		// 16 GiB under a 19 GiB CPU tier can never start.
		{name: "shared memory below the CPU tier", overrides: `{"shm_bytes":17179869184}`, quick: `{"kv_offloading_size":19}`, code: "SHM_BELOW_KV_CPU_TIER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:invalid_create_"+strings.ReplaceAll(tc.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{deploymentFlatFixture()}); err != nil {
				t.Fatal(err)
			}
			template, err := st.InferenceModelByModelID("acme/deploy")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.RegisterNode("invalid-create-node", store.HashToken("token"), "invalid-create-node"); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus("invalid-create-node", proto.NodeStatus{Sys: proto.SysInfo{Hostname: "invalid-create-node", GPUVendor: "nvidia", Framework: "cuda"}, GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
				t.Fatal(err)
			}
			quick := ""
			if tc.quick != "" {
				quick = `,"quick_params":` + tc.quick
			}
			body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","node_uuid":"invalid-create-node"%s,"container_overrides":%s}`, template.ID, quick, tc.overrides)
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create status/body=%d %s", rec.Code, rec.Body.String())
			}
			if tc.code != "" {
				var wire struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				if wire.Code != tc.code || fmt.Sprint(wire.Params["shm_gib"]) != "16" || fmt.Sprint(wire.Params["tier_gib"]) != "19" {
					t.Fatalf("create body = %s, want code %s with shm_gib 16 and tier_gib 19", rec.Body.String(), tc.code)
				}
			}
			models, err := st.ListModels()
			if err != nil || len(models) != 0 {
				t.Fatalf("invalid launch registered model: %+v err=%v", models, err)
			}
			deployments, err := st.ListDeployments()
			if err != nil || len(deployments) != 0 {
				t.Fatalf("invalid launch persisted deployment: %+v err=%v", deployments, err)
			}
			instances, err := st.ListInstances()
			if err != nil || len(instances) != 0 {
				t.Fatalf("invalid launch persisted instance: %+v err=%v", instances, err)
			}
		})
	}
}
