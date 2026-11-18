package handler

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatReasoningPrecedesContentWithoutInventedTokenCounts(t *testing.T) {
	const thought = "想一想\nreasoning \"quoted\""
	server := httptest.NewServer(New(Options{Model: "reasoner", ReasoningText: thought, ResponseText: "answer", StreamChunks: 2, PromptTokens: 19, CompletionTokens: 23, TPOTDelay: time.Millisecond}))
	defer server.Close()
	events := readCompletionEvents(t, post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"reasoner","messages":[],"stream":true,"stream_options":{"include_usage":true,"continuous_usage_stats":true}}`)))
	var reasoning, content strings.Builder
	contentStarted := false
	finished := false
	for _, event := range events {
		choices := event["choices"].([]any)
		if len(choices) == 0 {
			if !finished {
				t.Fatal("usage preceded terminal stop")
			}
			u := event["usage"].(map[string]any)
			if u["prompt_tokens"] != float64(19) || u["completion_tokens"] != float64(23) || u["total_tokens"] != float64(42) {
				t.Fatalf("synthetic configured totals changed: %v", u)
			}
			for _, key := range []string{"reasoning_tokens", "completion_tokens_details"} {
				if _, found := u[key]; found {
					t.Fatalf("unconfigured reasoning accounting invented: %v", u)
				}
			}
			continue
		}
		choice := choices[0].(map[string]any)
		if choice["finish_reason"] == "stop" {
			finished = true
			continue
		}
		delta := choice["delta"].(map[string]any)
		if _, found := delta["reasoning"]; found {
			t.Fatal("do not double-report two reasoning aliases")
		}
		if value, ok := delta["reasoning_content"].(string); ok {
			if contentStarted {
				t.Fatal("reasoning after final content")
			}
			reasoning.WriteString(value)
		}
		if value, ok := delta["content"].(string); ok {
			contentStarted = true
			content.WriteString(value)
		}
	}
	if reasoning.String() != thought || content.String() != "answeranswer" || !finished {
		t.Fatalf("reasoning=%q content=%q finished=%v", reasoning.String(), content.String(), finished)
	}
}

func TestChatCancellationDuringReasoningReleasesGeneration(t *testing.T) {
	engine := New(Options{ReasoningText: "thinking", ResponseText: "not yet", TPOTDelay: 3 * time.Second})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(finished); engine.ServeHTTP(w, r) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true,"stream_options":{"include_usage":true}}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	observed := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"reasoning_content":"thinking"`) {
			observed = true
			break
		}
	}
	if !observed {
		t.Fatalf("reasoning frame missing before cancellation: %v", scanner.Err())
	}
	cancel()
	response.Body.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceling during reasoning left generation sleeping")
	}
}
