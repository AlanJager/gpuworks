package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The tokenizer endpoints answer the way vLLM 0.30's do (checked against a
// live vLLM serving Qwen3-0.6B): POST only at the root, /tokenize with a
// prompt or chat messages and /detokenize with token ids; the model may be
// left out and, when given, must be the served one. The fixture's tokenizer is
// byte-level: each byte of the text is one token whose id is the byte value,
// so detokenize inverts tokenize exactly and an id past 255 decodes to
// nothing, as an id the vocabulary lacks does in vLLM.

// defaultMaxModelLen is the max_model_len tokenize reports.
const defaultMaxModelLen = 40960

type tokenizeRequest struct {
	Model    string          `json:"model"`
	Prompt   json.RawMessage `json:"prompt"`
	Messages json.RawMessage `json:"messages"`
	// AddGenerationPrompt defaults to true for chat messages, as vLLM's does.
	AddGenerationPrompt *bool           `json:"add_generation_prompt"`
	ChatTemplateKwargs  json.RawMessage `json:"chat_template_kwargs"`
	AddSpecialTokens    *bool           `json:"add_special_tokens"`
	ReturnTokenStrs     bool            `json:"return_token_strs"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func registerTokenize(mux *http.ServeMux, opts Options) {
	served := func(model string) bool {
		if model == "" {
			return true
		}
		names := []string{opts.Model}
		if opts.Models != nil {
			names = opts.Models()
		}
		for _, name := range names {
			if name == "" || name == model {
				return true
			}
		}
		return false
	}
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		if !requirePOSTLikeFastAPI(w, r) {
			return
		}
		var request tokenizeRequest
		if !decodeVLLMBody(w, r, &request) {
			return
		}
		var text string
		switch {
		case isPresent(request.Messages):
			var messages []chatMessage
			if err := json.Unmarshal(request.Messages, &messages); err != nil {
				writeVLLMValidation(w, "body.messages", "list_type", "Input should be a valid list")
				return
			}
			if len(messages) == 0 {
				writeVLLMError(w, http.StatusBadRequest, "BadRequestError", nil, "Cannot apply chat template to an empty conversation. Provide at least one message.")
				return
			}
			rendered, ok := renderChat(messages, request.AddGenerationPrompt == nil || *request.AddGenerationPrompt)
			if !ok {
				writeVLLMValidation(w, "body.messages", "string_type", "Input should be a valid string")
				return
			}
			text = rendered
		case isPresent(request.Prompt):
			if err := json.Unmarshal(request.Prompt, &text); err != nil {
				writeVLLMValidation(w, "body.prompt", "string_type", "Input should be a valid string")
				return
			}
		default:
			param := "body.prompt"
			writeVLLMError(w, http.StatusBadRequest, "Bad Request", &param,
				"2 validation errors:\n  {'type': 'missing', 'loc': 'body.prompt', 'msg': 'Field required'}\n  {'type': 'missing', 'loc': 'body.messages', 'msg': 'Field required'}")
			return
		}
		if !served(request.Model) {
			writeModelNotFound(w, request.Model)
			return
		}
		writeRawJSON(w, tokenizeText(text, request.ReturnTokenStrs))
	})
	mux.HandleFunc("/detokenize", func(w http.ResponseWriter, r *http.Request) {
		if !requirePOSTLikeFastAPI(w, r) {
			return
		}
		var request struct {
			Model  string            `json:"model"`
			Tokens []json.RawMessage `json:"tokens"`
		}
		if !decodeVLLMBody(w, r, &request) {
			return
		}
		if request.Tokens == nil {
			writeVLLMValidation(w, "body.tokens", "missing", "Field required")
			return
		}
		var text bytes.Buffer
		for i, raw := range request.Tokens {
			var id int
			if err := json.Unmarshal(raw, &id); err != nil {
				writeVLLMValidation(w, fmt.Sprintf("body.tokens.%d", i), "int_parsing", "Input should be a valid integer")
				return
			}
			if id >= 0 && id < 256 {
				text.WriteByte(byte(id))
			}
		}
		if !served(request.Model) {
			writeModelNotFound(w, request.Model)
			return
		}
		writeRawJSON(w, struct {
			Prompt string `json:"prompt"`
		}{text.String()})
	})
}

// tokenizeResponse is vLLM's /tokenize reply, its keys in vLLM's order.
// TokenStrs is null unless return_token_strs asked for them.
type tokenizeResponse struct {
	Count       int      `json:"count"`
	MaxModelLen int      `json:"max_model_len"`
	Tokens      []int    `json:"tokens"`
	TokenStrs   []string `json:"token_strs"`
}

func tokenizeText(text string, withStrings bool) tokenizeResponse {
	out := tokenizeResponse{MaxModelLen: defaultMaxModelLen, Tokens: make([]int, 0, len(text))}
	if withStrings {
		out.TokenStrs = make([]string, 0, len(text))
	}
	for _, b := range []byte(text) {
		out.Tokens = append(out.Tokens, int(b))
		if withStrings {
			out.TokenStrs = append(out.TokenStrs, tokenString(b))
		}
	}
	out.Count = len(out.Tokens)
	return out
}

// renderChat applies a ChatML template, the shape of Qwen's: one block per
// message and, when asked, the assistant's opening.
func renderChat(messages []chatMessage, generationPrompt bool) (string, bool) {
	var out strings.Builder
	for _, m := range messages {
		content, ok := messageText(m.Content)
		if !ok {
			return "", false
		}
		out.WriteString("<|im_start|>" + m.Role + "\n" + content + "<|im_end|>\n")
	}
	if generationPrompt {
		out.WriteString("<|im_start|>assistant\n")
	}
	return out.String(), true
}

// messageText reads a message's content: a string, or a list of parts whose
// text parts are joined.
func messageText(raw json.RawMessage) (string, bool) {
	if !isPresent(raw) {
		return "", true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String(), true
}

// tokenString is how a byte token reads: itself when printable ASCII, else
// the byte-fallback spelling.
func tokenString(b byte) string {
	if b >= 0x20 && b < 0x7f {
		return string(rune(b))
	}
	return fmt.Sprintf("<0x%02X>", b)
}

func isPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// requirePOSTLikeFastAPI answers any other method the way vLLM's FastAPI
// router does: 405 with its own detail body, not the OpenAI error shape.
func requirePOSTLikeFastAPI(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", http.MethodPost)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte(`{"detail":"Method Not Allowed"}`))
	return false
}

// decodeVLLMBody reads the JSON body; vLLM does not insist on a content type.
func decodeVLLMBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeVLLMValidation(w, "body", "json_invalid", "JSON decode error")
		return false
	}
	return true
}

func writeVLLMValidation(w http.ResponseWriter, loc, kind, msg string) {
	writeVLLMError(w, http.StatusBadRequest, "Bad Request", &loc,
		fmt.Sprintf("1 validation error:\n  {'type': '%s', 'loc': '%s', 'msg': '%s'}", kind, loc, msg))
}

func writeModelNotFound(w http.ResponseWriter, model string) {
	param := "model"
	writeVLLMError(w, http.StatusNotFound, "NotFoundError", &param, "The model `"+model+"` does not exist.")
}

// writeVLLMError is vLLM's error body: the OpenAI shape with the status
// repeated as code.
func writeVLLMError(w http.ResponseWriter, status int, errorType string, param *string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	type body struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    int     `json:"code"`
	}
	_ = encoder.Encode(struct {
		Error body `json:"error"`
	}{body{message, errorType, param, status}})
}

// writeRawJSON writes v without escaping <, > and &: vLLM sends token strings
// such as <|im_start|> as they are.
func writeRawJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
}
