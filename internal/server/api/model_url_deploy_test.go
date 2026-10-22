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
	"gpuworks.ai/gpuworks/pkg/bench"
)

// A directory model must flow through deployment evaluation the same way a
// repository model does: it becomes a public download candidate, named by the
// host that actually serves it, because "url" alone does not tell two mirrors
// apart.
func TestEvaluateOffersADirectoryModelAsAPublicDownloadNamedByItsHost(t *testing.T) {
	st, err := store.Open("file:api_evaluate_url_candidate?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B",
		Name: "Qwen3-0.6B", WeightBytes: 1 << 30, Arch: "Qwen3ForCausalLM",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("url-candidate-node", store.HashToken("token"), "url-candidate-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("url-candidate-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "url-candidate-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA GPU", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate",
		strings.NewReader(fmt.Sprintf(`{"model_id":%d,"node_uuid":"url-candidate-node","engine":"vllm"}`, model.ID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Acquisition dto.AcquisitionResult `json:"acquisition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var found *dto.AcquisitionCandidate
	for index, candidate := range response.Acquisition.Providers {
		if candidate.Key == "public:url:Qwen/Qwen3-0.6B:" {
			found = &response.Acquisition.Providers[index]
		}
	}
	if found == nil {
		t.Fatalf("no public:url candidate in %+v", response.Acquisition.Providers)
	}
	if found.Label != "url" || found.DisplayName != "hf-mirror.example.com" {
		t.Fatalf("candidate = %+v, want it named by the serving host", found)
	}
}

// A benchmark has no tokenizer scheme for a directory model, so the reference
// is left empty and the run proceeds on the existing fallback rather than
// being rejected.
func TestBenchmarkLeavesTheTokenizerReferenceEmptyForADirectoryModel(t *testing.T) {
	for _, tc := range []struct {
		model *store.Model
		want  string
	}{
		{model: &store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B"}, want: "hf://Qwen/Qwen3-0.6B"},
		{model: &store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B"}, want: "ms://Qwen/Qwen3-0.6B"},
		// A plain file server serves no tokenizer by repository id, so the
		// reference stays empty and the run falls back rather than failing.
		{model: &store.Model{Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B"}},
		{model: &store.Model{Source: "local", SourceID: "Qwen3-0.6B"}},
		{model: nil},
	} {
		if got := tokenizerRefForModel(tc.model); got != tc.want {
			t.Errorf("tokenizerRefForModel(%+v) = %q, want %q", tc.model, got, tc.want)
		}
	}
	schemes := bench.Dictionary().TokenizerSchemes
	if _, ok := schemes["url"]; ok {
		t.Fatalf("the tokenizer scheme dictionary gained a url entry: %+v", schemes)
	}
	if schemes["huggingface"] != "hf" || schemes["modelscope"] != "ms" || schemes["upload"] != "file" {
		t.Fatalf("tokenizer schemes changed: %+v", schemes)
	}
}

// A directory model is matched by its repository id alone, which is what makes
// an inference-catalog preset apply to weights served from a private mirror.
func TestTemplateMatchingFindsADirectoryModelByRepositoryID(t *testing.T) {
	st, err := store.Open("file:api_template_match_url_model?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "qwen/qwen3-0.6b", DisplayTitle: "Qwen3 0.6B", ReleaseDate: "2025-04-03", Source: "upstream", Unsupported: `[]`,
		Specs: []store.InferenceSpecRow{{
			Name: "qwen3-0.6b-vllm", DisplayProfile: "general", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:v0.11.0", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"Qwen/Qwen3-0.6B"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=7.5","display_target":"nvidia"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}

	// The template's weight source names a Hugging Face repository; the model
	// is served from a mirror. They are the same weights, and the matcher must
	// say so.
	if !templateWeightMatchesModel(weightSource{Provider: "huggingface", RepoID: "Qwen/Qwen3-0.6B"}, model) {
		t.Fatal("a directory model was not matched by its repository id")
	}
	if templateWeightMatchesModel(weightSource{Provider: "huggingface", RepoID: "Qwen/Qwen3-4B"}, model) {
		t.Fatal("a different repository id was matched")
	}
	entry, err := st.InferenceModelByModelID(model.SourceID)
	if err != nil || entry == nil || entry.ModelID != "qwen/qwen3-0.6b" {
		t.Fatalf("catalog lookup by source id = %+v err = %v", entry, err)
	}
}
