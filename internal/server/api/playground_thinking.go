package api

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nonThinkingQwen3Markers are Qwen3 derivatives whose chat template does not
// read enable_thinking. Several still declare openai.chat (ASR, rerankers),
// and their ids contain the substring "qwen3".
var nonThinkingQwen3Markers = []string{"asr", "tts", "rerank", "embed"}

// playgroundThinkingToggleEnabled is true only when Disable Thinking is a
// reliable switch: vLLM and SGLang pass chat_template_kwargs through to a
// Qwen3 chat template. Other engines ignore that object, and other model
// families do not read enable_thinking from it.
func playgroundThinkingToggleEnabled(engine, servedName string, model *store.Model, hasChat bool) bool {
	if !hasChat || model == nil {
		return false
	}
	if engine != deploy.EngineVLLM && engine != deploy.EngineSGLang {
		return false
	}
	fields := []string{servedName, model.Name, model.SourceID, model.Arch}
	fields = append(fields, qwen3MetaFields(model.Meta)...)
	for _, field := range fields {
		if qwen3NonThinking(field) {
			return false
		}
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), "qwen3") {
			return true
		}
	}
	return false
}

func qwen3NonThinking(value string) bool {
	lower := strings.ToLower(value)
	if !strings.Contains(lower, "qwen3") {
		return false
	}
	for _, marker := range nonThinkingQwen3Markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// qwen3MetaFields returns the config identity strings. The raw config is not
// scanned: keys such as max_position_embeddings contain "embed" and would
// reject a normal Qwen3 chat model.
func qwen3MetaFields(meta string) []string {
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
