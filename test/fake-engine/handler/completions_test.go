package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readCompletionEvents(t *testing.T, response *http.Response) []map[string]any {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-cache" {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("SSE status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	scanner := bufio.NewScanner(response.Body)
	var events []map[string]any
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if done || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("unexpected SSE line %q after DONE=%v", line, done)
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("missing terminal [DONE]")
	}
	return events
}

func TestTextCompletionsStreamHTTPContract(t *testing.T) {
	for _, options := range []string{"", `,"stream_options":{"include_usage":false}`, `,"stream_options":{"include_usage":true}`} {
		t.Run(options, func(t *testing.T) {
			const text = "line one\n中文 \"quoted\""
			server := httptest.NewServer(New(Options{Model: "fixture-model", ResponseText: text, StreamChunks: 2, PromptTokens: 7, CompletionTokens: 11, CachedTokens: 2}))
			defer server.Close()
			events := readCompletionEvents(t, post(t, server.URL+"/v1/completions", "application/json", strings.NewReader(`{"model":"fixture-model","prompt":"Continue this","stream":true`+options+`}`)))
			var output strings.Builder
			finished, usageEvents := 0, 0
			for eventIndex, event := range events {
				if event["object"] != "text_completion" || event["model"] != "fixture-model" || event["id"] == nil || event["id"] == "" || event["created"] == nil {
					t.Fatalf("invalid envelope: %v", event)
				}
				choices, ok := event["choices"].([]any)
				if !ok {
					t.Fatalf("choices=%v", event)
				}
				if len(choices) == 0 {
					usageEvents++
					if finished != 1 || eventIndex != len(events)-1 {
						t.Fatalf("usage before final stop: %v", events)
					}
					u, ok := event["usage"].(map[string]any)
					if !ok || u["prompt_tokens"] != float64(7) || u["completion_tokens"] != float64(11) || u["total_tokens"] != float64(18) {
						t.Fatalf("configured usage changed: %v", event)
					}
					continue
				}
				if len(choices) != 1 {
					t.Fatalf("choices=%v", choices)
				}
				choice := choices[0].(map[string]any)
				if choice["index"] != float64(0) || choice["logprobs"] != nil {
					t.Fatalf("choice=%v", choice)
				}
				if choice["finish_reason"] == "stop" {
					finished++
					if choice["text"] != "" {
						t.Fatalf("terminal text=%v", choice)
					}
				} else {
					if finished > 0 || choice["finish_reason"] != nil {
						t.Fatalf("content after finish: %v", choice)
					}
					value, ok := choice["text"].(string)
					if !ok {
						t.Fatalf("text delta missing: %v", choice)
					}
					output.WriteString(value)
				}
			}
			expectedUsage := 0
			if strings.Contains(options, `:true`) {
				expectedUsage = 1
			}
			if output.String() != text+text || finished != 1 || usageEvents != expectedUsage {
				t.Fatalf("output=%q finish=%d usage=%d, expected usage=%d", output.String(), finished, usageEvents, expectedUsage)
			}
		})
	}
}

func TestCompletionHTTPRejections(t *testing.T) {
	for _, path := range []string{"/v1/completions", "/v1/chat/completions"} {
		for _, test := range []struct {
			name, body string
			status     int
		}{
			{"missing input", `{"stream":true}`, 400},
			{"wrong stream type", `{"prompt":"hi","messages":[],"stream":"yes"}`, 400},
			{"wrong continuous usage type", `{"prompt":"hi","messages":[],"stream":true,"stream_options":{"include_usage":true,"continuous_usage_stats":"yes"}}`, 400},
			{"wrong usage type", `{"prompt":"hi","messages":[],"stream":true,"stream_options":{"include_usage":"yes"}}`, 400},
			{"options without stream", `{"prompt":"hi","messages":[],"stream_options":{"include_usage":true}}`, 400},
			{"malformed", `{`, 400},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				server := httptest.NewServer(New(Options{Model: "fixture-model"}))
				defer server.Close()
				response := post(t, server.URL+path, "application/json", strings.NewReader(test.body))
				defer response.Body.Close()
				var body map[string]any
				_ = json.NewDecoder(response.Body).Decode(&body)
				if response.StatusCode != test.status || response.Header.Get("Content-Type") != "application/json" || body["error"] == nil {
					t.Fatalf("status=%d headers=%v body=%v", response.StatusCode, response.Header, body)
				}
			})
		}
		t.Run(path+"/status override", func(t *testing.T) {
			server := httptest.NewServer(New(Options{Model: "fixture-model", StatusOverride: 429}))
			defer server.Close()
			response := post(t, server.URL+path, "application/json", strings.NewReader(`{"prompt":"hi","messages":[],"stream":true}`))
			defer response.Body.Close()
			if response.StatusCode != 429 || response.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
			}
		})
	}
}

func TestCompletionCancellationReleasesHandler(t *testing.T) {
	for _, path := range []string{"/v1/completions", "/v1/chat/completions"} {
		for _, phase := range []string{"waiting", "streaming"} {
			t.Run(path+"/"+phase, func(t *testing.T) {
				options := Options{Model: "fixture-model", StreamChunks: 5, TPOTDelay: 3 * time.Second}
				if phase == "waiting" {
					options.TTFTDelay = 3 * time.Second
				}
				engine := New(options)
				started, finished := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(started)
					defer close(finished)
					engine.ServeHTTP(w, r)
				}))
				defer server.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(`{"prompt":"hi","messages":[],"stream":true}`))
				request.Header.Set("Content-Type", "application/json")
				results := make(chan *http.Response, 1)
				go func() { response, _ := http.DefaultClient.Do(request); results <- response }()
				<-started
				if phase == "streaming" {
					response := <-results
					if response == nil {
						t.Fatal("stream request failed before cancellation")
					}
					defer response.Body.Close()
					if response.Header.Get("Content-Type") != "text/event-stream" {
						t.Fatalf("not a stream: %v", response.Header)
					}
					scanner := bufio.NewScanner(response.Body)
					if !scanner.Scan() {
						t.Fatal("missing first delta")
					}
				}
				cancel()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("canceled client left generation handler sleeping")
				}
				if phase == "waiting" {
					if response := <-results; response != nil {
						response.Body.Close()
					}
				}
			})
		}
	}
}

func TestChatCompletionUsageIsOptInAndTerminal(t *testing.T) {
	for _, option := range []string{"", `,"stream_options":null`, `,"stream_options":{"include_usage":false}`, `,"stream_options":{"include_usage":true}`, `,"stream_options":{"include_usage":true,"continuous_usage_stats":true}`} {
		t.Run(option, func(t *testing.T) {
			server := httptest.NewServer(New(Options{Model: "fixture-model", ResponseText: "body", StreamChunks: 2, PromptTokens: 7, CompletionTokens: 11}))
			defer server.Close()
			events := readCompletionEvents(t, post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true`+option+`}`)))
			wantUsage := strings.Contains(option, `"include_usage":true`)
			var text strings.Builder
			finish, usageEvents := 0, 0
			for i, event := range events {
				if event["object"] != "chat.completion.chunk" || event["model"] != "fixture-model" {
					t.Fatalf("chat envelope=%v", event)
				}
				choices := event["choices"].([]any)
				if len(choices) == 0 {
					usageEvents++
					if !wantUsage || i != len(events)-1 || finish != 1 {
						t.Fatalf("usage order=%d finish=%d event=%v", i, finish, event)
					}
					usage := event["usage"].(map[string]any)
					if usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(11) {
						t.Fatalf("usage=%v", usage)
					}
					continue
				}
				if wantUsage {
					if value, found := event["usage"]; !found || value != nil {
						t.Fatalf("nonterminal usage=%v", event)
					}
				}
				choice := choices[0].(map[string]any)
				if choice["finish_reason"] == "stop" {
					finish++
					continue
				}
				if finish != 0 {
					t.Fatal("content after finish")
				}
				delta := choice["delta"].(map[string]any)
				if value, ok := delta["content"].(string); ok {
					text.WriteString(value)
				}
			}
			expectedUsage := 0
			if wantUsage {
				expectedUsage = 1
			}
			if text.String() != "bodybody" || finish != 1 || usageEvents != expectedUsage {
				t.Fatalf("text=%q finish=%d usage=%d", text.String(), finish, usageEvents)
			}
		})
	}
}

func TestCompletionNonstreamDefaultsAndMethodContract(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		t.Run(path, func(t *testing.T) {
			server := httptest.NewServer(New(Options{Model: "fixture-model", StreamChunks: 5, PromptTokens: 7, CompletionTokens: 11}))
			defer server.Close()
			response := post(t, server.URL+path, "application/json", strings.NewReader(`{"messages":[],"prompt":"hi"}`))
			var body map[string]any
			err := json.NewDecoder(response.Body).Decode(&body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			choice := body["choices"].([]any)[0].(map[string]any)
			content := choice["text"]
			if path == "/v1/chat/completions" {
				message := choice["message"].(map[string]any)
				content = message["content"]
				if _, found := message["reasoning_content"]; found {
					t.Fatal("default chat unexpectedly enabled reasoning")
				}
			}
			if content != "hello from fake-engine" || choice["finish_reason"] != "stop" || body["usage"].(map[string]any)["total_tokens"] != float64(18) {
				t.Fatalf("default nonstream=%v", body)
			}
			response, err = http.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 405 || response.Header.Get("Allow") != "POST" || response.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("method contract=%v", response)
			}
		})
	}
}
