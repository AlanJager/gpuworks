package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sseServer replies to every request with the next scripted SSE body, the
// way an OpenAI-compatible endpoint streams: data frames, keep-alive
// comments, a usage chunk with no choices, then [DONE].
func sseServer(t *testing.T, bodies ...string) (*httptest.Server, *[]ChatRequestWire) {
	t.Helper()
	var mu sync.Mutex
	var seen []ChatRequestWire
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequestWire
		_ = decodeJSON(r, &req)
		mu.Lock()
		seen = append(seen, req)
		i := n
		n++
		mu.Unlock()
		if i >= len(bodies) {
			http.Error(w, `{"error":{"message":"unexpected call"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(bodies[i]))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// ChatRequestWire is the request as the endpoint receives it, stream options
// included.
type ChatRequestWire struct {
	ChatRequest
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func frames(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		if strings.HasPrefix(l, ":") {
			b.WriteString(l + "\n\n")
			continue
		}
		b.WriteString("data: " + l + "\n\n")
	}
	return b.String()
}

const answerFrames = `{"choices":[{"delta":{"role":"assistant","reasoning_content":"thinking "}}]}` + "\n" +
	`{"choices":[{"delta":{"content":"Root "}}]}` + "\n" +
	`{"choices":[{"delta":{"content":"cause"},"finish_reason":"stop"}]}` + "\n" +
	`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":3}}` + "\n" +
	`[DONE]`

func TestChatStreamAssemblesTextReasoningAndUsage(t *testing.T) {
	srv, seen := sseServer(t, frames(append([]string{": keep-alive"}, strings.Split(answerFrames, "\n")...)...))
	var text, reasoning []string
	msg, usage, err := NewClient(Endpoint{BaseURL: srv.URL, APIKey: "k", Model: "m"}, "127.0.0.1").ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(d string) { text = append(text, d) }, func(d string) { reasoning = append(reasoning, d) })
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Root cause" || msg.Role != "assistant" || len(msg.ToolCalls) != 0 {
		t.Fatalf("msg = %+v", msg)
	}
	if strings.Join(text, "|") != "Root |cause" || strings.Join(reasoning, "|") != "thinking " {
		t.Fatalf("text %q reasoning %q", text, reasoning)
	}
	if usage != (Usage{PromptTokens: 40, CompletionTokens: 3}) {
		t.Fatalf("usage = %+v", usage)
	}
	req := (*seen)[0]
	if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage || req.Model != "m" {
		t.Fatalf("request = %+v", req)
	}
}

// Tool calls arrive in fragments: an id and name first, arguments split
// across chunks, two calls interleaved by index.
func TestChatStreamReassemblesFragmentedToolCalls(t *testing.T) {
	srv, _ := sseServer(t, frames(
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"get_instance_events","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"search_logs","arguments":"{\"instance_"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"uuid\":\"i1\"}"}},{"index":1,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	))
	msg, _, err := NewClient(Endpoint{BaseURL: srv.URL}, "127.0.0.1").ChatStream(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	a, b := msg.ToolCalls[0], msg.ToolCalls[1]
	if a.ID != "a" || a.Function.Name != "search_logs" || a.Function.Arguments != `{"instance_uuid":"i1"}` || a.Type != "function" {
		t.Fatalf("call 0 = %+v", a)
	}
	if b.ID != "b" || b.Function.Name != "get_instance_events" || b.Function.Arguments != "{}" {
		t.Fatalf("call 1 = %+v", b)
	}
}

func TestChatStreamErrors(t *testing.T) {
	cut, _ := sseServer(t, frames(`{"choices":[{"delta":{"content":"half an ans"}}]}`))
	if _, _, err := NewClient(Endpoint{BaseURL: cut.URL}, "127.0.0.1").ChatStream(context.Background(), nil, nil, nil, nil); !errors.Is(err, errStreamCut) {
		t.Errorf("cut stream err = %v", err)
	}
	inband, _ := sseServer(t, frames(`{"error":{"message":"context length exceeded"}}`))
	if _, _, err := NewClient(Endpoint{BaseURL: inband.URL}, "127.0.0.1").ChatStream(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("in-band error = %v", err)
	}
	garbage, _ := sseServer(t, frames(`{not json`))
	if _, _, err := NewClient(Endpoint{BaseURL: garbage.URL}, "127.0.0.1").ChatStream(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "llm stream") {
		t.Errorf("malformed frame err = %v", err)
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer limited.Close()
	if _, _, err := NewClient(Endpoint{BaseURL: limited.URL}, "127.0.0.1").ChatStream(context.Background(), nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "llm http 429") {
		t.Errorf("http error = %v", err)
	}
}

// A streamed run reports each tool call as it starts and ends, the answer
// text as it arrives, and when the first text came.
func TestRunStreamObservesStepsAndText(t *testing.T) {
	srv, seen := sseServer(t,
		frames(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"get_gpu_topology","arguments":"{\"node_uuid\":\"n1\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`[DONE]`,
		),
		frames(strings.Split(answerFrames, "\n")...),
	)
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL}, "127.0.0.1"), newMockEnv()).RunStream(context.Background(), "sys", "seed", DefaultMaxSteps, Observer{
		Step: func(id int, s Step, running bool) {
			if running {
				note("start:" + s.Tool)
			} else {
				note("end:" + s.Tool)
			}
		},
		Text:      func(d string) { note("text:" + d) },
		Reasoning: func(d string) { note("think:" + d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "start:get_gpu_topology,end:get_gpu_topology,think:thinking ,text:Root ,text:cause"
	if got := strings.Join(log, ","); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if res.Answer != "Root cause" || res.Calls != 2 || len(res.Steps) != 1 || res.Usage.PromptTokens != 50 {
		t.Fatalf("res = %+v", res)
	}
	if res.FirstTextMS <= 0 || res.TotalMS < res.FirstTextMS {
		t.Fatalf("first text %d total %d", res.FirstTextMS, res.TotalMS)
	}
	if len((*seen)[1].Messages) != 4 || (*seen)[1].Messages[3].Role != "tool" {
		t.Fatalf("tool result not fed back: %+v", (*seen)[1].Messages)
	}
}

func TestRunStreamPropagatesStreamFailure(t *testing.T) {
	srv, _ := sseServer(t, frames(`{"choices":[{"delta":{"content":"partial"}}]}`))
	var got string
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL}, "127.0.0.1"), newMockEnv()).RunStream(context.Background(), "sys", "seed", DefaultMaxSteps, Observer{Text: func(d string) { got += d }})
	if !errors.Is(err, errStreamCut) || got != "partial" || res.Calls != 1 || res.FirstTextMS <= 0 {
		t.Fatalf("err %v text %q res %+v", err, got, res)
	}
}

func decodeJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// vLLM 0.34 streams reasoning under "reasoning", some gateways under
// "thinking"; a transitional vLLM sends the same text under two names.
func TestChatStreamReadsEveryReasoningFieldOnce(t *testing.T) {
	for _, delta := range []string{`{"reasoning":"think"}`, `{"thinking":"think"}`, `{"reasoning_content":"think","reasoning":"think"}`} {
		srv, _ := sseServer(t, frames(`{"choices":[{"delta":`+delta+`}]}`, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`, `[DONE]`))
		var reasoning []string
		msg, _, err := NewClient(Endpoint{BaseURL: srv.URL, APIKey: "k", Model: "m"}, "127.0.0.1").ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
			nil, func(d string) { reasoning = append(reasoning, d) })
		if err != nil || msg.Content != "ok" || strings.Join(reasoning, "|") != "think" {
			t.Fatalf("delta %s: msg=%+v reasoning=%q err=%v", delta, msg, reasoning, err)
		}
	}
}
