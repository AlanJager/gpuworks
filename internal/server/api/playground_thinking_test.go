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
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func playgroundPresetByKey(t *testing.T, presets []dto.PlaygroundExtraPreset, key string) dto.PlaygroundExtraPreset {
	t.Helper()
	for _, preset := range presets {
		if preset.Key == key {
			return preset
		}
	}
	t.Fatalf("preset %q missing from %+v", key, presets)
	return dto.PlaygroundExtraPreset{}
}

func encodedPresetValue(t *testing.T, preset dto.PlaygroundExtraPreset) string {
	t.Helper()
	if preset.Value == nil {
		return ""
	}
	encoded, err := json.Marshal(preset.Value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestPlaygroundDisableThinkingPresetCarriesTheFamilyKey pins the preset the
// page applies: for a model whose family is declared, the chat-template field
// that turns thinking off, written as chat_template_kwargs and never as a
// top-level field the engine would ignore.
func TestPlaygroundDisableThinkingPresetCarriesTheFamilyKey(t *testing.T) {
	cases := []struct {
		family string
		model  *store.Model
		wire   string
	}{
		{"qwen3", &store.Model{Name: "Qwen3 0.6B", SourceID: "Qwen/Qwen3-0.6B", Meta: `{"model_type":"qwen3"}`}, `{"chat_template_kwargs":{"enable_thinking":false}}`},
		{"deepseek-v4", &store.Model{Name: "DeepSeek-V4.1-Flash", SourceID: "deepseek-ai/DeepSeek-V4.1-Flash", Meta: `{"model_type":"deepseek_v41_text"}`}, `{"chat_template_kwargs":{"thinking":false}}`},
	}
	for _, tc := range cases {
		t.Run(tc.family, func(t *testing.T) {
			decided := playgroundThinkingSwitchFor(deploy.EngineVLLM, tc.model.Name, tc.model, true)
			if !decided.declared() || decided.Family != tc.family {
				t.Fatalf("switch = %+v, want family %q", decided, tc.family)
			}
			preset := playgroundPresetByKey(t, playgroundPresets(decided), "disable_thinking")
			if preset.AppliesWhen != tc.family || preset.Unavailable != nil {
				t.Fatalf("preset = %+v", preset)
			}
			if wire := encodedPresetValue(t, preset); wire != tc.wire {
				t.Fatalf("value wire = %s, want %s", wire, tc.wire)
			}
			if _, topLevel := preset.Value[tc.model.Name]; topLevel {
				t.Fatal("preset wrote a top-level thinking field")
			}
			if _, topLevel := preset.Value["enable_thinking"]; topLevel {
				t.Fatal("preset wrote a top-level enable_thinking field")
			}
		})
	}
}

// TestPlaygroundDisableThinkingPresetStaysOffWhenUndeclared covers the other
// half of the bug: a model no family covers must get no value at all (so the
// page cannot enable a switch the template ignores) and a text that names the
// parameters the reader can add by hand, without claiming thinking cannot be
// stopped.
func TestPlaygroundDisableThinkingPresetStaysOffWhenUndeclared(t *testing.T) {
	llama := &store.Model{Name: "Llama", SourceID: "meta/llama", Meta: `{"model_type":"llama"}`}
	decided := playgroundThinkingSwitchFor(deploy.EngineVLLM, "llama", llama, true)
	if decided.Unavailable != playgroundThinkingUnavailableModel {
		t.Fatalf("switch = %+v, want the model reason", decided)
	}
	preset := playgroundPresetByKey(t, playgroundPresets(decided), "disable_thinking")
	if preset.Value != nil || preset.AppliesWhen != "" {
		t.Fatalf("undeclared model got an enabled preset: %+v", preset)
	}
	examples := []string{`{"chat_template_kwargs":{"enable_thinking":false}}`, `{"chat_template_kwargs":{"thinking":false}}`}
	for _, language := range requiredPlaygroundLocales {
		text := preset.Unavailable[language]
		if text == "" {
			t.Fatalf("locale %s has no unavailable text", language)
		}
		for _, example := range examples {
			if !strings.Contains(text, example) {
				t.Fatalf("locale %s text does not name %s: %s", language, example, text)
			}
		}
		if strings.Contains(text, "{{examples}}") {
			t.Fatalf("locale %s text still carries the placeholder: %s", language, text)
		}
		for _, claim := range []string{"hiding the thinking", "藏起"} {
			if strings.Contains(text, claim) {
				t.Fatalf("locale %s text still claims thinking cannot be stopped: %s", language, text)
			}
		}
	}
	// The low reasoning effort preset is a plain example every model can send,
	// so it keeps its value and carries no unavailable text.
	lowEffort := playgroundPresetByKey(t, playgroundPresets(playgroundThinkingSwitch{}), "low_reasoning_effort")
	if encodedPresetValue(t, lowEffort) != `{"reasoning_effort":"low"}` || lowEffort.Unavailable != nil || lowEffort.AppliesWhen != "" {
		t.Fatalf("reasoning preset = %+v", lowEffort)
	}
}

// TestPlaygroundDisableThinkingExplainsAnUnadaptedEngine separately pins the
// other reason: an engine that drops chat_template_kwargs cannot be fixed by
// adding a parameter, so its text must not promise the parameter will work.
func TestPlaygroundDisableThinkingExplainsAnUnadaptedEngine(t *testing.T) {
	qwen := &store.Model{Name: "Qwen3 0.6B", SourceID: "Qwen/Qwen3-0.6B", Meta: `{"model_type":"qwen3"}`}
	decided := playgroundThinkingSwitchFor(deploy.EngineCallable, "Qwen/Qwen3-0.6B", qwen, true)
	if decided.Unavailable != playgroundThinkingUnavailableEngine {
		t.Fatalf("switch = %+v, want the engine reason", decided)
	}
	preset := playgroundPresetByKey(t, playgroundPresets(decided), "disable_thinking")
	if preset.Value != nil {
		t.Fatalf("unadapted engine got a value: %+v", preset)
	}
	modelText := playgroundPresets(playgroundThinkingSwitchFor(deploy.EngineVLLM, "llama", &store.Model{Name: "Llama", Meta: `{"model_type":"llama"}`}, true))[1].Unavailable
	for _, language := range requiredPlaygroundLocales {
		// The two reasons are different facts, so they must not share a text.
		if preset.Unavailable[language] == "" || preset.Unavailable[language] == modelText[language] {
			t.Fatalf("locale %s engine reason text = %q", language, preset.Unavailable[language])
		}
	}
	if !strings.Contains(preset.Unavailable[localization.English], "does not serve Chat through an engine") {
		t.Fatalf("engine text = %q", preset.Unavailable[localization.English])
	}
}

// TestPlaygroundThinkingSwitchDecision is the capability table read through the
// decision: engine × model family × chat or not.
func TestPlaygroundThinkingSwitchDecision(t *testing.T) {
	qwen := &store.Model{Name: "Qwen3 0.6B", SourceID: "Qwen/Qwen3-0.6B", Arch: "Qwen3ForCausalLM", Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`}
	deepseek := &store.Model{Name: "DeepSeek-V4.1-Flash", SourceID: "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", Arch: "dense", Meta: `{"model_type":"deepseek_v41_text"}`}
	llama := &store.Model{Name: "Llama", SourceID: "meta/llama", Arch: "LlamaForCausalLM", Meta: `{"model_type":"llama"}`}
	// Derive a model from another by overriding its identity fields.
	with := func(base *store.Model, name, source, arch, meta string) *store.Model {
		return &store.Model{Name: name, SourceID: source, Arch: arch, Meta: meta}
	}
	cases := []struct {
		name   string
		engine string
		model  *store.Model
		served string
		chat   bool
		want   string // declared family, or "" for not declared
		key    string
	}{
		{"qwen3 on vLLM", deploy.EngineVLLM, qwen, "Qwen/Qwen3-0.6B", true, "qwen3", "enable_thinking"},
		{"qwen3 on SGLang", deploy.EngineSGLang, qwen, "served", true, "qwen3", "enable_thinking"},
		{"deepseek v4 on vLLM", deploy.EngineVLLM, deepseek, "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", true, "deepseek-v4", "thinking"},
		{"deepseek v4 on SGLang", deploy.EngineSGLang, deepseek, "served-name-is-user-chosen", true, "deepseek-v4", "thinking"},
		{"deepseek v4 named by model_type only", deploy.EngineVLLM, with(deepseek, "served", "acme/custom", "", `{"model_type":"deepseek_v4"}`), "served", true, "deepseek-v4", "thinking"},
		// The engines that do not forward chat_template_kwargs.
		{"qwen3 on callable", deploy.EngineCallable, qwen, "Qwen/Qwen3-0.6B", true, "", ""},
		{"deepseek v4 on callable", deploy.EngineCallable, deepseek, "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", true, "", ""},
		// A deployment that serves no chat operation has no chat template.
		{"qwen3 without chat", deploy.EngineVLLM, qwen, "Qwen/Qwen3-0.6B", false, "", ""},
		{"deepseek v4 without chat", deploy.EngineVLLM, deepseek, "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", false, "", ""},
		// Families with no measured chat-template thinking switch stay out.
		{"llama", deploy.EngineVLLM, llama, "llama", true, "", ""},
		{"deepseek v3.2", deploy.EngineVLLM, with(deepseek, "DeepSeek-V3.2", "deepseek-ai/DeepSeek-V3.2", "", `{"model_type":"deepseek_v32"}`), "deepseek-v32", true, "", ""},
		{"deepseek r1", deploy.EngineVLLM, with(deepseek, "DeepSeek-R1", "deepseek-ai/DeepSeek-R1", "", `{"model_type":"deepseek_v3"}`), "deepseek-r1", true, "", ""},
		// Derivatives of a declared family serve chat, but their template has
		// no thinking mode.
		{"qwen3 ASR", deploy.EngineVLLM, with(qwen, "Qwen3-ASR-1.7B", "Qwen/Qwen3-ASR-1.7B", "dense", `{"model_type":"qwen3_asr","architectures":["Qwen3ASRForConditionalGeneration"]}`), "Qwen/Qwen3-ASR-1.7B", true, "", ""},
		{"qwen3 reranker", deploy.EngineSGLang, with(qwen, "Qwen3-Reranker-8B", "Qwen/Qwen3-Reranker-8B", "Qwen3ForCausalLM", `{"model_type":"qwen3"}`), "rerank", true, "", ""},
		{"qwen3 embedding", deploy.EngineVLLM, with(qwen, "Qwen3-Embedding-0.6B", "Qwen/Qwen3-Embedding-0.6B", "Qwen3ForCausalLM", `{"model_type":"qwen3"}`), "emb", true, "", ""},
		{"qwen3 TTS", deploy.EngineVLLM, with(qwen, "voice", "acme/voice", "dense", `{"model_type":"qwen3_tts"}`), "voice", true, "", ""},
		{"deepseek OCR", deploy.EngineVLLM, with(deepseek, "DeepSeek-OCR", "deepseek-ai/DeepSeek-OCR", "dense", `{"model_type":"deepseek_vl_v2","max_position_embeddings":8192}`), "deepseek-ocr", true, "", ""},
		{"deepseek VL2", deploy.EngineVLLM, with(deepseek, "DeepSeek-VL2", "deepseek-ai/deepseek-vl2", "dense", `{"model_type":"deepseek_vl_v2"}`), "deepseek-vl2", true, "", ""},
		// The served name is user-chosen and cannot enable or disable the
		// switch: only the spec identity decides.
		{"served name cannot enable a switch", deploy.EngineVLLM, llama, "deepseek-ai/DeepSeek-V4.1-Flash", true, "", ""},
		{"served name cannot spoil a declared family", deploy.EngineVLLM, qwen, "deepseek-v4-embedding", true, "qwen3", "enable_thinking"},
		// max_position_embeddings contains "embed"; only the decoded identity
		// fields may match a marker (GWKS-138 keeps that rule).
		{"qwen3.5 with long context", deploy.EngineVLLM, with(qwen, "small", "acme/small", "dense", `{"model_type":"qwen3_5","max_position_embeddings":262144}`), "small", true, "qwen3", "enable_thinking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decided := playgroundThinkingSwitchFor(tc.engine, tc.served, tc.model, tc.chat)
			if decided.Family != tc.want || decided.ChatTemplateKey != tc.key {
				t.Fatalf("switch = %+v, want family %q key %q", decided, tc.want, tc.key)
			}
			if got := decided.value(); (got == nil) != (tc.want == "") {
				t.Fatalf("value = %#v for declared family %q", got, tc.want)
			}
			// An undeclared switch always names a reason, and a declared one
			// never does: the page shows exactly one of value or text.
			if declared := decided.Unavailable == ""; declared != (tc.want != "") {
				t.Fatalf("switch = %+v, want a reason only when undeclared", decided)
			}
		})
	}
}

// TestPlaygroundThinkingFamilyTableIsOrderedAndUnique keeps the table usable as
// a precedence list: one row per family, and a family declared once.
func TestPlaygroundThinkingFamilyTableIsOrderedAndUnique(t *testing.T) {
	seenNames := map[string]bool{}
	seenKeys := map[string]string{}
	for _, family := range playgroundThinkingFamilies {
		if family.Name == "" || family.ChatTemplateKey == "" || len(family.Markers) == 0 {
			t.Fatalf("incomplete row %+v", family)
		}
		if seenNames[family.Name] {
			t.Fatalf("duplicate family %q", family.Name)
		}
		seenNames[family.Name] = true
		if owner, ok := seenKeys[family.ChatTemplateKey]; ok {
			t.Fatalf("families %q and %q share the chat-template key %q", owner, family.Name, family.ChatTemplateKey)
		}
		seenKeys[family.ChatTemplateKey] = family.Name
	}
	// The unavailable text is generated from this table, so every declared
	// family is offered as a hand-written example.
	examples := playgroundDisableThinkingExamples()
	for _, family := range playgroundThinkingFamilies {
		if !strings.Contains(examples, `"`+family.ChatTemplateKey+`":false`) {
			t.Fatalf("examples %q do not name family %q", examples, family.Name)
		}
	}
}

func TestPlaygroundCatalogServesThinkingPresetPerModel(t *testing.T) {
	st, err := store.Open("file:playground_thinking_toggle_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	addModel := func(name, source, meta, operations string) *store.Model {
		t.Helper()
		model, err := st.UpsertModel(&store.Model{
			Source: "huggingface", SourceID: source, Revision: "main", Name: name, Meta: meta,
			Operations: operations, InputModalities: `["text"]`,
		})
		if err != nil {
			t.Fatal(err)
		}
		return model
	}
	qwen := addModel("Qwen3", "Qwen/Qwen3-0.6B", `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`, `[{"type":"openai.chat"}]`)
	deepseek := addModel("DeepSeek-V4.1-Flash", "deepseek-ai/DeepSeek-V4.1-Flash", `{"model_type":"deepseek_v41_text"}`, `[{"type":"openai.chat"}]`)
	llama := addModel("Llama", "meta/llama", `{"model_type":"llama"}`, `[{"type":"openai.chat"}]`)
	node, _ := st.RegisterNode("thinking-node", store.HashToken("token"), "thinking-node")
	add := func(model *store.Model, served, engine string, port int) {
		t.Helper()
		dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: served, State: "active", Engine: engine})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: served, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: port}); err != nil {
			t.Fatal(err)
		}
	}
	add(qwen, "Qwen/Qwen3-0.6B", deploy.EngineVLLM, 30000)
	add(deepseek, "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji", deploy.EngineVLLM, 30001)
	// callable does not forward chat_template_kwargs, so its preset stays off.
	add(llama, "llama", deploy.EngineCallable, 30002)

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
	byID := map[string]dto.PlaygroundModel{}
	for _, model := range catalog.Models {
		byID[model.ID] = model
	}
	if len(byID) != 3 {
		t.Fatalf("models = %+v", byID)
	}
	wire := map[string]string{
		"Qwen/Qwen3-0.6B": `{"chat_template_kwargs":{"enable_thinking":false}}`,
		"deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji": `{"chat_template_kwargs":{"thinking":false}}`,
	}
	applies := map[string]string{"Qwen/Qwen3-0.6B": "qwen3", "deepseek-ai/DeepSeek-V4.1-Flash-1017-zdji": "deepseek-v4"}
	for id, want := range wire {
		presets := byID[id].ExtraParameterPresets
		if len(presets) != 2 {
			t.Fatalf("%s presets = %+v", id, presets)
		}
		disable := playgroundPresetByKey(t, presets, "disable_thinking")
		if got := encodedPresetValue(t, disable); got != want {
			t.Fatalf("%s disable thinking wire = %s, want %s", id, got, want)
		}
		if disable.AppliesWhen != applies[id] || disable.Unavailable != nil {
			t.Fatalf("%s disable thinking preset = %+v", id, disable)
		}
		if playgroundPresetByKey(t, presets, "low_reasoning_effort").Value["reasoning_effort"] != "low" {
			t.Fatalf("%s lost the reasoning preset", id)
		}
	}
	// An engine that drops chat_template_kwargs keeps the button visible but
	// disabled, with the text naming what to add by hand.
	llamaDisable := playgroundPresetByKey(t, byID["llama"].ExtraParameterPresets, "disable_thinking")
	if llamaDisable.Value != nil || llamaDisable.AppliesWhen != "" {
		t.Fatalf("llama disable thinking = %+v", llamaDisable)
	}
	llamaText := llamaDisable.Unavailable[localization.English]
	if !strings.Contains(llamaText, `"thinking":false`) || !strings.Contains(llamaText, `"enable_thinking":false`) {
		t.Fatalf("llama unavailable text = %q", llamaText)
	}
	if llamaDisable.Unavailable[localization.Chinese] == "" {
		t.Fatalf("llama unavailable text lacks Chinese: %+v", llamaDisable.Unavailable)
	}
	// The catalog no longer carries one flat preset list: whether a preset can
	// be honored depends on the deployment.
	if strings.Contains(rec.Body.String(), `"extra_parameter_presets":[`) && !strings.Contains(rec.Body.String(), `"id":"Qwen/Qwen3-0.6B"`) {
		t.Fatalf("catalog wire = %s", rec.Body.String())
	}
}
