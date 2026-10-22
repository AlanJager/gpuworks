package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The field report: a tiered engine (Strata) with 63 GiB of weights on one
// 48 GiB A40, deployed with the node named by hand. Sized on its weights the
// deployment can never fit that card and the create is refused outright; with
// the per-card admission contract the template's resolver declares, it is
// created. A copy that lost the contract (the old template editor dropped the
// field that carried it) was refused in manual mode too.
func TestManualCreateSizesATieredEngineByItsTemplateContract(t *testing.T) {
	st, err := store.Open("file:template_contract_fit?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("a40", store.HashToken("token"), "a40"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("a40", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "a40", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA A40", Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6", MemoryTotalBytes: 48 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	const weightBytes = 68026093024
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF", ArtifactSubfolder: "IQ2_XS", Name: "strata", WeightBytes: weightBytes})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := json.Marshal(map[string]string{"language": "starlark", "resolver": `def resolve(gpu):
    if gpu.min_memory_gib < 12:
        return reject("Strata requires at least 12 GiB VRAM on every card")
    return accept(gpu_memory_mib_per_device=12288)
`})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(name, placement string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Image: "example/vllm:test", Quantization: "gsq-rco iq2_xs",
			WeightBytes: weightBytes, Placement: placement,
			WeightSources: `[{"provider":"huggingface","repo_id":"ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF","subfolder":"IQ2_XS"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=7.5"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/tiered", DisplayTitle: "Tiered", Source: "upstream", Architecture: "qwen4", ContextLength: 32768,
		Specs: []store.InferenceSpecRow{spec("contract", string(contract)), spec("no-contract", "")},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/tiered")
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	create := func(specName string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":%q,"engine":"vllm","image":"example/vllm:test","node_uuid":"a40","placement_mode":"manual","quick_params":{}}`,
			specName, model.ID, template.ID, specName)
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	if status, body := create("no-contract"); status != http.StatusBadRequest || !strings.Contains(body, ErrorDeploymentNeverFits) {
		t.Fatalf("63 GiB of weights on a 48 GiB card without a contract = %d %s", status, body)
	}
	if status, body := create("contract"); status != http.StatusCreated {
		t.Fatalf("the 12 GiB contract on a 48 GiB card = %d %s", status, body)
	}
}
