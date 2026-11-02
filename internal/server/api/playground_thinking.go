package api

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// playgroundChatTemplateKwargsEngines pass a request's chat_template_kwargs
// object through to the chat template. No other engine does, so on them no
// chat-template thinking switch can work.
var playgroundChatTemplateKwargsEngines = []string{deploy.EngineVLLM, deploy.EngineSGLang}

// playgroundNonThinkingMarkers are derivatives of a declared family whose chat
// template has no thinking story: ASR, TTS, reranker and embedding
// checkpoints. Several still declare openai.chat, and their ids (and even
// their architectures, for a reranker) carry the family name.
var playgroundNonThinkingMarkers = []string{"asr", "tts", "rerank", "embed"}

// playgroundThinkingFamily declares how one model family turns thinking off:
// the chat_template_kwargs field its chat template reads, always set to false.
type playgroundThinkingFamily struct {
	// Name identifies the family towards the client
	// (PlaygroundExtraPreset.AppliesWhen).
	Name string
	// ChatTemplateKey is the chat_template_kwargs field that turns thinking
	// off.
	ChatTemplateKey string
	// Markers are lowercase substrings of a model identity that name the
	// family.
	Markers []string
}

// playgroundThinkingFamilies is the declared capability table: one row per
// model family whose chat template reads a thinking switch. Adding a family is
// a row here, never a branch in the decision below or in the Playground UI.
//
// Table order is the precedence: when one identity names two families the
// first row wins, so the answer never depends on which identity field was read
// first. The families below do not overlap, so the order is only a tie-break.
//
// A family stays out of this table until someone measures the key it reads.
// Guessing would recreate the bug this table fixes, the other way round: an
// enabled switch whose parameter the template ignores. For a model without a
// declared family the Playground instead names the parameters it could add by
// hand (see playgroundDisableThinkingExamples).
var playgroundThinkingFamilies = []playgroundThinkingFamily{
	// Qwen3 reads chat_template_kwargs.enable_thinking (GWKS-18: a top-level
	// enable_thinking field is accepted and ignored).
	{Name: "qwen3", ChatTemplateKey: "enable_thinking", Markers: []string{"qwen3"}},
	// DeepSeek-V4.x reads chat_template_kwargs.thinking. Measured on
	// deepseek-ai/DeepSeek-V4.1-Flash: reasoning_tokens 96 -> 0 and a simple
	// question falls from 29 tokens to 2, so thinking really stops instead of
	// being hidden. The markers are the two spellings a spec carries the family
	// in: model_type deepseek_v41_text and the served id DeepSeek-V4.1-Flash.
	// DeepSeek-V3.x and R1 stay out: neither template is measured to read a
	// thinking switch.
	{Name: "deepseek-v4", ChatTemplateKey: "thinking", Markers: []string{"deepseek_v4", "deepseek-v4"}},
}

// Reasons a family-dependent preset cannot be honored. Each has its own text,
// because the truthful explanation differs: an engine that never forwards
// chat_template_kwargs cannot be fixed by adding a parameter, while an
// undeclared model family can.
const (
	// playgroundThinkingUnavailableEngine: the deployment does not serve chat
	// through an engine that forwards chat_template_kwargs.
	playgroundThinkingUnavailableEngine = "engine"
	// playgroundThinkingUnavailableModel: the chat template's thinking switch
	// is not declared for this model.
	playgroundThinkingUnavailableModel = "model"
)

// playgroundThinkingUnavailableReasons lists every reason, so the locale must
// carry a text for each.
var playgroundThinkingUnavailableReasons = []string{playgroundThinkingUnavailableEngine, playgroundThinkingUnavailableModel}

// playgroundThinkingSwitch is the decided switch for one deployment: the family
// that declares it and the chat-template field that stops thinking. Without a
// declared family it carries the reason instead.
type playgroundThinkingSwitch struct {
	Family          string
	ChatTemplateKey string
	// Unavailable is the reason no family is declared; empty when one is.
	Unavailable string
}

func (s playgroundThinkingSwitch) declared() bool { return s.ChatTemplateKey != "" }

// value is the extra-parameters patch that turns thinking off, or nil when no
// family is declared, so a preset the server cannot honor carries no value.
func (s playgroundThinkingSwitch) value() map[string]any {
	if !s.declared() {
		return nil
	}
	return map[string]any{"chat_template_kwargs": map[string]any{s.ChatTemplateKey: false}}
}

// playgroundThinkingSwitchFor decides whether Disable Thinking is a reliable
// switch for one deployment. It needs an engine that forwards
// chat_template_kwargs, a chat operation, and a chat template the declared
// table knows.
func playgroundThinkingSwitchFor(engine, servedName string, model *store.Model, hasChat bool) playgroundThinkingSwitch {
	if !hasChat || !playgroundForwardsChatTemplateKwargs(engine) {
		return playgroundThinkingSwitch{Unavailable: playgroundThinkingUnavailableEngine}
	}
	if model != nil {
		identities := playgroundModelIdentities(servedName, model)
		// An ASR, TTS, reranker or embedding checkpoint of a declared family
		// serves chat, but its template has no thinking mode.
		if !containsAnyMarker(identities, playgroundNonThinkingMarkers) {
			// Table order is the precedence, so the answer does not depend on
			// which identity field happened to be read first.
			for _, family := range playgroundThinkingFamilies {
				if containsAnyMarker(identities, family.Markers) {
					return playgroundThinkingSwitch{Family: family.Name, ChatTemplateKey: family.ChatTemplateKey}
				}
			}
		}
	}
	return playgroundThinkingSwitch{Unavailable: playgroundThinkingUnavailableModel}
}

func playgroundForwardsChatTemplateKwargs(engine string) bool {
	for _, candidate := range playgroundChatTemplateKwargsEngines {
		if engine == candidate {
			return true
		}
	}
	return false
}

// containsAnyMarker reports whether any identity carries one of the markers.
func containsAnyMarker(identities, markers []string) bool {
	for _, identity := range identities {
		for _, marker := range markers {
			if strings.Contains(identity, marker) {
				return true
			}
		}
	}
	return false
}

// playgroundModelIdentities are the lowercase strings a model spec carries its
// family name in. The served name is deliberately not one of them: it is chosen
// by whoever creates the deployment, so letting it enable a wire parameter
// would let a name like "deepseek-v4-test" switch on a parameter its real
// template ignores. It is only read when the spec carries no identity at all.
func playgroundModelIdentities(servedName string, model *store.Model) []string {
	fields := []string{model.Name, model.SourceID, model.Arch}
	fields = append(fields, playgroundMetaFields(model.Meta)...)
	identities := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			identities = append(identities, strings.ToLower(trimmed))
		}
	}
	if len(identities) == 0 {
		return []string{strings.ToLower(servedName)}
	}
	return identities
}

// playgroundMetaFields returns the config identity strings. The raw config is
// not scanned: keys such as max_position_embeddings contain "embed" and would
// reject a normal chat model.
func playgroundMetaFields(meta string) []string {
	var decoded struct {
		ModelType     string   `json:"model_type"`
		Architectures []string `json:"architectures"`
	}
	if json.Unmarshal([]byte(meta), &decoded) != nil {
		return nil
	}
	fields := make([]string, 0, 1+len(decoded.Architectures))
	if decoded.ModelType != "" {
		fields = append(fields, decoded.ModelType)
	}
	return append(fields, decoded.Architectures...)
}

// playgroundDisableThinkingExamples lists the chat_template_kwargs patches the
// declared families use, so the text of an unsupported model can name the
// parameter to add by hand instead of claiming thinking cannot be stopped. It
// follows the table, so a new family appears here without a second edit.
func playgroundDisableThinkingExamples() string {
	examples := make([]string, 0, len(playgroundThinkingFamilies))
	for _, family := range playgroundThinkingFamilies {
		encoded, err := json.Marshal(playgroundThinkingSwitch{Family: family.Name, ChatTemplateKey: family.ChatTemplateKey}.value())
		if err != nil {
			continue
		}
		examples = append(examples, string(encoded))
	}
	return strings.Join(examples, ", ")
}
