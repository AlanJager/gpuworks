package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func registerCompletions(mux *http.ServeMux, opts Options, stats *engineStats, logf func(string, ...any)) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		chat := path == "/v1/chat/completions"
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !requireMethod(w, r, http.MethodPost) {
				return
			}
			if override := stats.statusOverride.Load(); override != 0 {
				logf("Forcing status override %d for a completion request", override)
				writeAPIError(w, int(override), "server_error", "fake-engine: forced status override")
				return
			}
			var request struct {
				Model         string `json:"model"`
				Stream        bool   `json:"stream"`
				StreamOptions *struct {
					IncludeUsage bool `json:"include_usage"`
					// Accepted for UI request compatibility; this fixture has
					// configured final totals, not per-delta token observations.
					ContinuousUsageStats bool `json:"continuous_usage_stats"`
				} `json:"stream_options"`
				Messages json.RawMessage `json:"messages"`
				Prompt   json.RawMessage `json:"prompt"`
			}
			if err := decodeJSON(r, &request); err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			input, name := request.Prompt, "prompt"
			if chat {
				input, name = request.Messages, "messages"
			}
			if len(input) == 0 || string(input) == "null" {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", name+" is required")
				return
			}
			if request.StreamOptions != nil && !request.Stream {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "stream_options requires stream=true")
				return
			}
			requestID := fmt.Sprintf("req-%08x", stats.requests.Add(1))
			logf("Received request %s: prompt=%d tok, completion=%d tok", requestID, opts.PromptTokens, opts.CompletionTokens)
			stats.waiting.Add(1)
			ready := waitGeneration(r.Context(), opts.TTFTDelay)
			stats.waiting.Add(-1)
			if !ready {
				return
			}
			stats.running.Add(1)
			defer stats.running.Add(-1)
			model := opts.Model
			if model == "" {
				model = request.Model
			}
			if request.Stream {
				includeUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
				if !streamCompletion(w, r, opts, chat, model, includeUsage) {
					return
				}
			} else {
				answer := completionEnvelope(chat, false, model)
				choice := map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil}
				if chat {
					message := map[string]string{"role": "assistant", "content": opts.ResponseText}
					if opts.ReasoningText != "" {
						message["reasoning_content"] = opts.ReasoningText
					}
					choice["message"] = message
				} else {
					choice["text"] = opts.ResponseText
				}
				answer["choices"], answer["usage"] = []any{choice}, usage(opts)
				writeJSON(w, answer)
			}
			// These are explicitly configured synthetic totals for a completed reply,
			// never inferred from bytes, SSE chunks, or a canceled partial response.
			stats.promptTokens.Add(int64(opts.PromptTokens))
			stats.generationTokens.Add(int64(opts.CompletionTokens))
			logf("Generated response for %s", requestID)
		})
	}
}

func completionEnvelope(chat, stream bool, model string) map[string]any {
	object := "text_completion"
	if chat {
		object = "chat.completion"
		if stream {
			object += ".chunk"
		}
	}
	return map[string]any{"id": "fake-engine-completion", "object": object, "created": time.Now().Unix(), "model": model}
}

func streamCompletion(w http.ResponseWriter, r *http.Request, opts Options, chat bool, model string, includeUsage bool) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	envelope := completionEnvelope(chat, true, model)
	send := func(choices []any, usageValue any) bool {
		envelope["choices"] = choices
		if includeUsage {
			envelope["usage"] = usageValue
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			return false
		}
		if r.Context().Err() != nil {
			return false
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return true
	}
	choice := func(text string, finish any) map[string]any {
		value := map[string]any{"index": 0, "finish_reason": finish, "logprobs": nil}
		if chat {
			delta := map[string]any{}
			if finish == nil {
				delta["content"] = text
			}
			value["delta"] = delta
		} else {
			value["text"] = text
		}
		return value
	}
	if chat && !send([]any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil, "logprobs": nil}}, nil) {
		return false
	}
	if chat && opts.ReasoningText != "" {
		reasoning := map[string]any{"index": 0, "delta": map[string]string{"reasoning_content": opts.ReasoningText}, "finish_reason": nil, "logprobs": nil}
		if !send([]any{reasoning}, nil) || !waitGeneration(r.Context(), opts.TPOTDelay) {
			return false
		}
	}
	for i := 0; i < max(opts.StreamChunks, 1); i++ {
		if !send([]any{choice(opts.ResponseText, nil)}, nil) || !waitGeneration(r.Context(), opts.TPOTDelay) {
			return false
		}
	}
	if !send([]any{choice("", "stop")}, nil) {
		return false
	}
	if includeUsage && !send([]any{}, usage(opts)) {
		return false
	}
	if r.Context().Err() != nil {
		return false
	}
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return err == nil
}

func waitGeneration(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
