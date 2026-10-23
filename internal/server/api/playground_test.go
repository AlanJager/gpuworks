package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestPlaygroundCatalogIncludesDeploymentsWithoutAssistantConfigs(t *testing.T) {
	st, err := store.Open("file:playground_catalog_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "acme/asr", Revision: "main", Name: "ASR",
		Meta: `{"architectures":["Qwen3ASRForConditionalGeneration"],"audio_config":{"model_type":"whisper"}}`, ContextLength: 32768,
		Operations: `[{"type":"openai.audio.transcribe"}]`, InputModalities: `["audio"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := st.RegisterNode("playground-node", store.HashToken("token"), "playground-node")
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "qwen-asr", State: "active", Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "asr-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: 1, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	emptyModel, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/empty", Revision: "main", Name: "empty", Operations: `[]`, InputModalities: `[]`})
	emptyDep, _ := st.CreateDeployment(&store.Deployment{ModelID: emptyModel.ID, NodeID: node.ID, ServedName: "empty", State: "active", Engine: "vllm"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "empty-instance", DeploymentID: emptyDep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAIConfig(&store.AIConfig{Name: "Poe", BaseURL: "https://api.poe.com/v1", Model: "GPT-5"}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("playground-catalog-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Parameters) != 7 || len(catalog.ExtraParameterPresets) != 2 || len(catalog.Models) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	for _, parameter := range catalog.Parameters {
		if parameter.Label[localization.English] == "" || parameter.Label[localization.Chinese] == "" {
			t.Fatalf("parameter %q has incomplete labels: %#v", parameter.Name, parameter.Label)
		}
	}
	for _, preset := range catalog.ExtraParameterPresets {
		if preset.Label[localization.English] == "" || preset.Label[localization.Chinese] == "" {
			t.Fatalf("preset has incomplete labels: %#v", preset.Label)
		}
	}
	// The stream_options the client adds are part of the dictionary, on by
	// default, so the panel can show and switch them off.
	if !strings.Contains(rec.Body.String(), `"stream_options":[{"name":"include_usage","label":{"en":"Report usage (include_usage)","zh":"返回用量（include_usage）"},`) ||
		!strings.Contains(rec.Body.String(), `"default":true},{"name":"continuous_usage_stats",`) || !strings.Contains(rec.Body.String(), `"default":true,"requires":"include_usage"}]`) {
		t.Fatalf("stream options wire = %s", rec.Body.String())
	}
	if len(catalog.StreamOptions) != 2 || catalog.StreamOptions[1].Description[localization.Chinese] == "" {
		t.Fatalf("stream options = %+v", catalog.StreamOptions)
	}
	if catalog.Parameters[0].Name != "temperature" || catalog.Parameters[0].Default != float64(0.7) || catalog.ExtraParameterPresets[0].Value["reasoning_effort"] != "low" {
		t.Fatalf("playground semantic defaults changed: %+v / %+v", catalog.Parameters[0], catalog.ExtraParameterPresets[0])
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != inferenceapi.OperationAudioTranscribe || !catalog.Models[0].Sleeping || catalog.Models[0].ID != "qwen-asr" || !catalog.Models[0].PublicDataPlane {
		t.Fatalf("deployment model = %+v", catalog.Models[0])
	}
	if len(catalog.Models[0].Operations[0].Discovery) != 2 || len(catalog.Models[0].Operations[0].PrimaryInputs) != 1 || catalog.Models[0].Operations[0].PrimaryInputs[0] != "file" {
		t.Fatalf("sleeping model lost discovery/inputs: %+v", catalog.Models[0].Operations)
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.audio.transcribe" || catalog.Models[0].Operations[0].Endpoint != "/v1/audio/transcriptions" || catalog.Models[0].Operations[0].Renderer != "audio" {
		t.Fatalf("deployment operations = %+v", catalog.Models[0].Operations)
	}
	if strings.Contains(rec.Body.String(), "external:") || strings.Contains(rec.Body.String(), "GPT-5") {
		t.Fatalf("assistant configuration leaked into Playground: %s", rec.Body)
	}
}

func TestPlaygroundCatalogUsesCurrentModelOperations(t *testing.T) {
	st, err := store.Open("file:playground_snapshot_contract_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/video", Revision: "main", Name: "video", Meta: `{"architectures":["CausalLM"]}`, Operations: `[{"type":"openai.video.generate"}]`, InputModalities: `["text"]`})
	node, _ := st.RegisterNode("snapshot-node", store.HashToken("token"), "snapshot-node")
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "video", State: "active", Engine: "sglang"})
	contract := json.RawMessage(`{"health_endpoint":"/health","operations":[{"type":"openai.embeddings"}]}`)
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: contract}})
	_, _ = st.CreateInstance(&store.Instance{UUID: "snapshot-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)})
	signer, _ := auth.NewTokenSigner("snapshot-contract-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.video.generate" {
		t.Fatalf("catalog=%+v", catalog.Models)
	}
}

func TestPlaygroundCatalogDeduplicatesRolloutsAndPrefersServingDeployment(t *testing.T) {
	st, err := store.Open("file:playground_rollout_dedupe_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/video", Revision: "main", Name: "video", Operations: `[{"type":"openai.video.generate"}]`, InputModalities: `["text"]`})
	node, _ := st.RegisterNode("rollout-node", store.HashToken("token"), "rollout-node")
	serving, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", State: "active", Engine: "sglang"})
	contract := json.RawMessage(`{"health_endpoint":"/health","operations":[{"type":"openai.embeddings"}]}`)
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: contract}})
	_, _ = st.CreateInstance(&store.Instance{UUID: "serving-instance", DeploymentID: serving.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)})
	// Written as a row from before served names were unique: a create now
	// refuses the name the serving deployment holds.
	pending := &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", State: "active", Engine: "vllm"}
	if err := st.DB().Create(pending).Error; err != nil {
		t.Fatal(err)
	}
	_, _ = st.CreateInstance(&store.Instance{UUID: "pending-instance", DeploymentID: pending.ID, NodeID: node.ID, State: proto.InstStarting})

	signer, _ := auth.NewTokenSigner("rollout-dedupe-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].DeploymentID != serving.ID {
		t.Fatalf("catalog models=%+v, want one serving rollout", catalog.Models)
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.video.generate" {
		t.Fatalf("catalog contract came from pending rollout: %+v", catalog.Models[0].Operations)
	}
}
