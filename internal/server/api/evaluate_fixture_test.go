package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateFixturesDir holds one file per representative evaluation, generated
// from a real handler against a seeded store. The deploy form's tests render
// these files and nothing else, so a server answer the form depends on cannot
// drift without failing here first.
const evaluateFixturesDir = "../../../ui/tests/fixtures/evaluate"

// evaluateFixturesEnv regenerates the fixtures instead of comparing them.
const evaluateFixturesEnv = "GPUWORKS_UPDATE_UI_FIXTURES"

func evaluateFixtureWorld(t *testing.T) (*apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:evaluate_fixture_world?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	// The fixtures are what a started server answers: its system catalog is in place.
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	register := func(uuid string, status proto.NodeStatus) {
		t.Helper()
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
	h100 := func(index int) proto.GPUInfo {
		return proto.GPUInfo{Index: index, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}
	}
	register("fixture-node", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "fixture-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"}, Network: testManagement("10.0.0.1"),
		GPUs: []proto.GPUInfo{h100(0), h100(1)},
	})
	register("fixture-chip-old", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "fixture-chip-old", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 40 << 30, Healthy: true}},
	})
	register("fixture-peer", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "fixture-peer", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"}, Network: testManagement("10.0.0.2"),
		GPUs: []proto.GPUInfo{h100(0), h100(1)},
	})
	if _, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "Acme/Deploy-FP8", WeightBytes: 16 << 30}); err != nil {
		t.Fatal(err)
	}
	catalog := deploymentFlatFixture()
	catalog.ModelID = "acme/fixture"
	catalog.Specs = append(catalog.Specs,
		store.InferenceSpecRow{Name: "sglang-balanced", Scenario: "balanced", Engine: "sglang", Image: "lmsysorg/sglang:latest", Quantization: "fp8", WeightBytes: 16 << 30,
			WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"},{"provider":"huggingface","repo_id":"Acme/Deploy-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=8.9"}`},
		store.InferenceSpecRow{Name: "callable-serve", Scenario: "balanced", Engine: "callable", Image: "acme/adapter:latest", Quantization: "fp8", WeightBytes: 16 << 30,
			WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=8.9"}`, Args: `["--model","${MODEL_PATH}","--host","${API_HOST}","--port","${API_PORT}","--adapter-threads","4"]`},
		store.InferenceSpecRow{Name: "future-hardware", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "fp8", WeightBytes: 16 << 30,
			WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=10.0"}`},
	)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st}}, template.ID
}

// TestEvaluateResponsesMatchUIFixtures captures one response per representative
// draft: a vLLM template, an SGLang template, a custom image, a callable spec,
// a multi-node draft, a draft with field errors, a draft no node is eligible
// for, and an automatic-placement draft. Regenerate with
// GPUWORKS_UPDATE_UI_FIXTURES=1; otherwise the files are compared byte for
// byte, so every field the form renders is pinned on the server side.
func TestEvaluateResponsesMatchUIFixtures(t *testing.T) {
	h, templateID := evaluateFixtureWorld(t)
	modelID := func() uint {
		models, _ := h.d.Store.ListModels()
		for _, model := range models {
			if model.SourceID == "Acme/Deploy-FP8" {
				return model.ID
			}
		}
		return 0
	}()
	cases := []struct {
		name string
		body string
	}{
		{"template_vllm", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":"fixture-node","target_vendor":"nvidia","quick_params":{"max_model_len":32768}}`, modelID, templateID)},
		{"template_sglang", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"sglang-balanced","node_uuid":"fixture-node"}`, modelID, templateID)},
		{"custom_image", fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","image":"private.example/vllm:custom","extra_args":"--load-format safetensors"}`, modelID)},
		{"callable", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"callable-serve","node_uuid":"fixture-node"}`, modelID, templateID)},
		{"multi_node", fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["fixture-node","fixture-peer"],"master_addr":"10.0.0.1","master_port":29500,"placement":{"tp":2}}`, modelID)},
		// A multi-error draft: the replica shape and the mount row are both
		// collected before the preview stops, so the fixture carries two field
		// errors on distinct controls.
		{"field_errors", fmt.Sprintf(`{"model_id":%d,"node_uuid":"fixture-node","engine":"vllm","replicas":-1,"container_overrides":{"extra_mounts":[{"host_path":"/elsewhere","container_path":"/model"}]}}`, modelID)},
		{"no_eligible_nodes", fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"future-hardware","engine":"vllm","placement_mode":"auto"}`, modelID, templateID)},
		{"auto_placement", fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID)},
	}
	update := os.Getenv(evaluateFixturesEnv) != ""
	if update {
		if err := os.MkdirAll(evaluateFixturesDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { checkEvaluateFixture(t, h, test.name, test.body, update) })
	}
	if update {
		t.Fatalf("regenerated fixtures under %s; rerun without %s", evaluateFixturesDir, evaluateFixturesEnv)
	}
}

// checkEvaluateFixture answers body with the real handler and compares the
// response with the fixture called name, or rewrites it when update is set.
func checkEvaluateFixture(t *testing.T, h *apiHandlers, name, body string, update bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	checkUIFixture(t, evaluateFixturesDir, name, rec.Body.Bytes(), update)
}

// checkUIFixture compares a real handler's JSON answer with the UI test
// fixture dir/name.json byte for byte, or rewrites it when update is set
// (GPUWORKS_UPDATE_UI_FIXTURES=1): the UI's tests render these files, so an
// answer the UI depends on cannot drift without failing here first.
func checkUIFixture(t *testing.T, dir, name string, body []byte, update bool) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	got := append(bytes.TrimRight(pretty.Bytes(), "\n"), '\n')
	path := filepath.Join(dir, name+".json")
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture (regenerate with %s=1 go test ./internal/server/api/ -run %s): %v", evaluateFixturesEnv, t.Name(), err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s drifted from %s; review the change and regenerate with %s=1 go test ./internal/server/api/ -run %s", name, path, evaluateFixturesEnv, t.Name())
	}
}
