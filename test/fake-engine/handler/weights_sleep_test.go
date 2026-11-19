package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Generated text, rather than created or other envelope metadata, makes a
// missing reload observable without freezing real response timestamps.
func TestDeepSleepReloadChangesGeneratedHTTPText(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		for _, stream := range []bool{false, true} {
			for _, wake := range []string{"plain", "weights-and-kv"} {
				name := path + "/json/" + wake
				if stream {
					name = path + "/sse/" + wake
				}
				t.Run(name, func(t *testing.T) {
					const normal = "normal deterministic answer"
					server := httptest.NewServer(New(Options{Model: "sleep-fixture", ResponseText: normal, StreamChunks: 2, PromptTokens: 7, CompletionTokens: 11}))
					defer server.Close()
					generate := func() string { return sleepCompletionText(t, server.URL, path, stream) }
					want := normal
					if stream {
						want += normal
					}
					if got := generate(); got != want {
						t.Fatalf("initial generated text=%q, want %q", got, want)
					}
					sleepCommand(t, server.URL+"/sleep?level=1", "", http.StatusOK)
					sleepCommand(t, server.URL+"/wake_up", "", http.StatusOK)
					if got := generate(); got != want {
						t.Fatalf("level-one wake damaged valid weights: %q", got)
					}
					sleepCommand(t, server.URL+"/sleep?level=2", "", http.StatusOK)
					if wake == "plain" {
						sleepCommand(t, server.URL+"/wake_up", "", http.StatusOK)
					} else {
						sleepCommand(t, server.URL+"/wake_up?tags=weights", "", http.StatusOK)
						sleepCommand(t, server.URL+"/wake_up?tags=kv_cache", "", http.StatusOK)
					}
					if got := generate(); got == want || !strings.Contains(strings.ToLower(got), "synthetic") {
						t.Fatalf("deep sleep without reload still generated normal text (or unmarked corruption): %q", got)
					}
					// Re-sleeping does not allocate weights. A reload before the weights
					// tag must fail, then the documented full sequence restores content.
					sleepCommand(t, server.URL+"/sleep?level=2", "", http.StatusOK)
					sleepCommand(t, server.URL+"/collective_rpc", `{"method":"reload_weights"}`, http.StatusInternalServerError)
					sleepCommand(t, server.URL+"/wake_up?tags=weights", "", http.StatusOK)
					response, err := http.Get(server.URL + "/is_sleeping")
					if err != nil {
						t.Fatal(err)
					}
					var state map[string]bool
					err = json.NewDecoder(response.Body).Decode(&state)
					response.Body.Close()
					if err != nil || !state["is_sleeping"] {
						t.Fatalf("weights alone unexpectedly woke kv buffers: %v %v", state, err)
					}
					sleepCommand(t, server.URL+"/collective_rpc", `{"method":"reload_weights"}`, http.StatusOK)
					sleepCommand(t, server.URL+"/wake_up?tags=kv_cache", "", http.StatusOK)
					if got := generate(); got != want {
						t.Fatalf("correct reload sequence did not restore text: %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func sleepCommand(t *testing.T, url, body string, want int) {
	t.Helper()
	response := post(t, url, "application/json", strings.NewReader(body))
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("%s status=%d want=%d", url, response.StatusCode, want)
	}
}

func sleepCompletionText(t *testing.T, base, path string, stream bool) string {
	t.Helper()
	body := `{"model":"sleep-fixture","messages":[],"prompt":"hi","stream":false}`
	if stream {
		body = `{"model":"sleep-fixture","messages":[],"prompt":"hi","stream":true,"stream_options":{"include_usage":true}}`
	}
	response := post(t, base+path, "application/json", strings.NewReader(body))
	var text strings.Builder
	checkUsage := func(value any) {
		usage, ok := value.(map[string]any)
		if !ok || usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(11) || usage["total_tokens"] != float64(18) {
			t.Fatalf("configured synthetic usage changed after sleep: %v", value)
		}
	}
	if stream {
		events := readCompletionEvents(t, response)
		finished, reported := false, false
		for _, event := range events {
			choices := event["choices"].([]any)
			if len(choices) == 0 {
				if !finished || reported {
					t.Fatalf("usage out of order: %v", event)
				}
				checkUsage(event["usage"])
				reported = true
				continue
			}
			choice := choices[0].(map[string]any)
			if choice["finish_reason"] == "stop" {
				finished = true
				continue
			}
			if finished {
				t.Fatalf("text after finish: %v", choice)
			}
			value := choice["text"]
			if path == "/v1/chat/completions" {
				value = choice["delta"].(map[string]any)["content"]
			}
			if content, ok := value.(string); ok {
				text.WriteString(content)
			}
		}
		if !finished || !reported {
			t.Fatalf("SSE lacked normal terminal contract: %v", events)
		}
	} else {
		defer response.Body.Close()
		var event map[string]any
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("invalid JSON success envelope: %v", response)
		}
		if err := json.NewDecoder(response.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["created"] == nil || event["model"] != "sleep-fixture" {
			t.Fatalf("envelope=%v", event)
		}
		choice := event["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish reason=%v", choice)
		}
		value := choice["text"]
		if path == "/v1/chat/completions" {
			value = choice["message"].(map[string]any)["content"]
		}
		content, ok := value.(string)
		if !ok {
			t.Fatalf("generated content absent: %v", choice)
		}
		text.WriteString(content)
		checkUsage(event["usage"])
	}
	return text.String()
}
