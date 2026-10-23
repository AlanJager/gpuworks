package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestPlaygroundDisableThinkingPresetUsesChatTemplateKwargs(t *testing.T) {
	parameters, presets := playgroundDictionary()
	if len(parameters) != 7 || len(presets) != 2 {
		t.Fatalf("dictionary = %d parameters, %d presets", len(parameters), len(presets))
	}
	disable := presets[1]
	if disable.Key != "disable_thinking" || disable.AppliesWhen != playgroundThinkingToggle {
		t.Fatalf("disable thinking preset = %+v", disable)
	}
	kwargs, _ := disable.Value["chat_template_kwargs"].(map[string]any)
	if len(disable.Value) != 1 || kwargs["enable_thinking"] != false {
		t.Fatalf("disable thinking value = %#v", disable.Value)
	}
	if _, topLevel := disable.Value["enable_thinking"]; topLevel {
		t.Fatal("disable thinking still sets a top-level enable_thinking field")
	}
	if disable.Unavailable[localization.English] == "" || disable.Unavailable[localization.Chinese] == "" {
		t.Fatalf("unavailable text = %#v", disable.Unavailable)
	}
	if presets[0].Key != "low_reasoning_effort" || presets[0].AppliesWhen != "" || presets[0].Unavailable != nil {
		t.Fatalf("reasoning preset = %+v", presets[0])
	}
}

func TestPlaygroundThinkingToggleRequiresQwen3ChatOnVLLMOrSGLang(t *testing.T) {
	qwen := &store.Model{Name: "Qwen3 0.6B", SourceID: "Qwen/Qwen3-0.6B", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`}
	if !playgroundThinkingToggleEnabled(deploy.EngineVLLM, "Qwen/Qwen3-0.6B", qwen, true) {
		t.Fatal("vLLM Qwen3 chat lost the thinking toggle")
	}
	if !playgroundThinkingToggleEnabled(deploy.EngineSGLang, "served", qwen, true) {
		t.Fatal("SGLang Qwen3 chat lost the thinking toggle")
	}
	if playgroundThinkingToggleEnabled(deploy.EngineCallable, "Qwen/Qwen3-0.6B", qwen, true) {
		t.Fatal("an unadapted engine was marked a reliable thinking switch")
	}
	if playgroundThinkingToggleEnabled(deploy.EngineVLLM, "Qwen/Qwen3-0.6B", qwen, false) {
		t.Fatal("a non-chat Qwen3 deployment was marked a thinking switch")
	}
	llama := &store.Model{Name: "Llama", SourceID: "meta/llama", Arch: "LlamaForCausalLM", Meta: `{"model_type":"llama"}`}
	if playgroundThinkingToggleEnabled(deploy.EngineVLLM, "llama", llama, true) {
		t.Fatal("Llama was marked a Qwen3 thinking switch")
	}
	// Qwen3.5 keeps the switch when the only identity is model_type qwen3_5.
	// max_position_embeddings must not look like an embedding model.
	qwen35 := &store.Model{Name: "small", SourceID: "acme/small", Arch: "dense", Meta: `{"model_type":"qwen3_5","max_position_embeddings":262144}`}
	if !playgroundThinkingToggleEnabled(deploy.EngineVLLM, "small", qwen35, true) {
		t.Fatal("qwen3_5 chat lost the thinking toggle")
	}
	// Catalog ASR and reranker specs declare openai.chat. Their ids contain
	// "qwen3", and a reranker's arch is still Qwen3ForCausalLM, but neither
	// chat template reads enable_thinking.
	asr := &store.Model{Name: "Qwen3-ASR-1.7B", SourceID: "Qwen/Qwen3-ASR-1.7B", Arch: "dense", Meta: `{"model_type":"qwen3_asr","architectures":["Qwen3ASRForConditionalGeneration"]}`}
	if playgroundThinkingToggleEnabled(deploy.EngineVLLM, "Qwen/Qwen3-ASR-1.7B", asr, true) {
		t.Fatal("Qwen3 ASR chat was marked a thinking switch")
	}
	rerank := &store.Model{Name: "Qwen3-Reranker-8B", SourceID: "Qwen/Qwen3-Reranker-8B", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":40960}`}
	if playgroundThinkingToggleEnabled(deploy.EngineSGLang, "rerank", rerank, true) {
		t.Fatal("Qwen3 reranker chat was marked a thinking switch")
	}
	embed := &store.Model{Name: "Qwen3-Embedding-0.6B", SourceID: "Qwen/Qwen3-Embedding-0.6B", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3"}`}
	if playgroundThinkingToggleEnabled(deploy.EngineVLLM, "emb", embed, true) {
		t.Fatal("Qwen3 embedding chat was marked a thinking switch")
	}
	tts := &store.Model{Name: "voice", SourceID: "acme/voice", Arch: "dense", Meta: `{"model_type":"qwen3_tts"}`}
	if playgroundThinkingToggleEnabled(deploy.EngineVLLM, "voice", tts, true) {
		t.Fatal("Qwen3 TTS chat was marked a thinking switch")
	}
}

func TestPlaygroundCatalogMarksQwen3ThinkingToggle(t *testing.T) {
	st, err := store.Open("file:playground_thinking_toggle_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	qwen, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", Name: "Qwen3",
		Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`,
		Operations: `[{"type":"openai.chat"}]`, InputModalities: `["text"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	llama, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "meta/llama", Revision: "main", Name: "Llama",
		Arch: "LlamaForCausalLM", Meta: `{"model_type":"llama"}`,
		Operations: `[{"type":"openai.chat"}]`, InputModalities: `["text"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := st.RegisterNode("thinking-node", store.HashToken("token"), "thinking-node")
	qwenDep, err := st.CreateDeployment(&store.Deployment{ModelID: qwen.ID, NodeID: node.ID, ServedName: "Qwen/Qwen3-0.6B", State: "active", Engine: deploy.EngineVLLM})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "qwen-instance", DeploymentID: qwenDep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	llamaDep, err := st.CreateDeployment(&store.Deployment{ModelID: llama.ID, NodeID: node.ID, ServedName: "llama", State: "active", Engine: deploy.EngineCallable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "llama-instance", DeploymentID: llamaDep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("playground-thinking-secret", time.Hour)
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
	if len(catalog.ExtraParameterPresets) != 2 || catalog.ExtraParameterPresets[1].Key != "disable_thinking" {
		t.Fatalf("presets = %+v", catalog.ExtraParameterPresets)
	}
	encoded, _ := json.Marshal(catalog.ExtraParameterPresets[1].Value)
	if string(encoded) != `{"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Fatalf("disable thinking wire = %s", encoded)
	}
	byID := map[string]dto.PlaygroundModel{}
	for _, model := range catalog.Models {
		byID[model.ID] = model
	}
	if !byID["Qwen/Qwen3-0.6B"].ThinkingToggle || byID["Qwen/Qwen3-0.6B"].Engine != deploy.EngineVLLM {
		t.Fatalf("qwen model = %+v", byID["Qwen/Qwen3-0.6B"])
	}
	if byID["llama"].ThinkingToggle {
		t.Fatalf("llama model = %+v", byID["llama"])
	}
}
