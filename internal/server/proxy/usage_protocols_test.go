package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

const (
	messagesPath  = "/v1/messages"
	responsesPath = "/v1/responses"
)

// Replies captured from vLLM 0.30 (gpu3, Qwen3 0.6B / Qwen3.5 0.8B) and from
// the SGLang build the production deployment runs (1f6ce4b0, GLM-5.3-Flash),
// fed in pieces of every size: the usage is the engine's whatever the reads
// split, the prompt adds up to the whole prompt, and the stream's reasoning
// and answer are told apart.
func TestObserverReadsAgentProtocolReplies(t *testing.T) {
	for _, tc := range []struct {
		fixture                    string
		prompt, completion, cached uint64
		cachedKnown                bool
		reasoning                  uint64
		reasoningKnown             bool
		finish                     string
		think, answer              bool
	}{
		// vLLM's Anthropic usage: input_tokens is the uncached part, the
		// cache figures add up to the prompt message_start reported.
		{fixture: "vllm-0.30-messages-plain.json", prompt: 19, completion: 6, cachedKnown: true, finish: "stop"},
		{fixture: "vllm-0.30-messages-stream.sse", prompt: 19, completion: 10, cachedKnown: true, finish: "stop", answer: true},
		{fixture: "vllm-0.30-messages-cache-write-stream.sse", prompt: 1706, completion: 40, cachedKnown: true, finish: "length", answer: true},
		{fixture: "vllm-0.30-messages-cached-stream.sse", prompt: 1706, completion: 40, cached: 1632, cachedKnown: true, finish: "length", answer: true},
		{fixture: "vllm-0.30-messages-think-stream.sse", prompt: 21, completion: 132, cachedKnown: true, finish: "stop", think: true, answer: true},
		{fixture: "vllm-0.30-messages-think.json", prompt: 21, completion: 105, cached: 16, cachedKnown: true, finish: "stop"},
		// SGLang reports the whole prompt in message_start and no cache
		// figure at all; its last message_delta has only the output.
		{fixture: "sglang-1f6ce4b0-messages-cached-stream.sse", prompt: 1576, completion: 40, finish: "length", think: true},
		{fixture: "sglang-1f6ce4b0-messages-think-stream.sse", prompt: 25, completion: 37, finish: "stop", think: true, answer: true},
		{fixture: "sglang-1f6ce4b0-messages-think.json", prompt: 25, completion: 48, finish: "stop"},
		// Responses: input_tokens is the whole prompt, the cached share and
		// the reasoning are nested.
		{fixture: "vllm-0.30-responses-plain.json", prompt: 19, completion: 3, cachedKnown: true, reasoningKnown: true, finish: "stop"},
		{fixture: "vllm-0.30-responses-stream.sse", prompt: 19, completion: 8, cachedKnown: true, reasoningKnown: true, finish: "stop", answer: true},
		// vLLM ends a reply cut at max_output_tokens with response.completed;
		// the response inside says incomplete.
		{fixture: "vllm-0.30-responses-cached-stream.sse", prompt: 1706, completion: 40, cached: 1632, cachedKnown: true, reasoningKnown: true, finish: "length", answer: true},
		{fixture: "vllm-0.30-responses-think-stream.sse", prompt: 21, completion: 117, cached: 16, cachedKnown: true, reasoning: 105, reasoningKnown: true, finish: "stop", think: true, answer: true},
		// SGLang ends it with response.incomplete.
		{fixture: "sglang-1f6ce4b0-responses-cached-stream.sse", prompt: 1576, completion: 38, cached: 1536, cachedKnown: true, reasoning: 40, reasoningKnown: true, finish: "length", think: true},
		{fixture: "sglang-1f6ce4b0-responses-think-stream.sse", prompt: 25, completion: 37, cachedKnown: true, reasoning: 30, reasoningKnown: true, finish: "stop", think: true, answer: true},
		{fixture: "sglang-1f6ce4b0-responses-think.json", prompt: 25, completion: 37, cachedKnown: true, reasoning: 29, reasoningKnown: true, finish: "stop"},
	} {
		path, header := messagesPath, sseHeader()
		if strings.Contains(tc.fixture, "-responses-") {
			path = responsesPath
		}
		if strings.HasSuffix(tc.fixture, ".json") {
			header = jsonHeader()
		}
		body := readFixture(t, tc.fixture)
		for seed := int64(1); seed <= 30; seed++ {
			o := newResponseObserver(path, 200, header, time.Unix(100, 0))
			if o == nil {
				t.Fatalf("%s: no observer", tc.fixture)
			}
			if seed == 1 {
				// One byte at a time splits every key there is.
				for i := 0; i < len(body); i++ {
					o.observe([]byte(body[i : i+1]))
				}
			} else {
				feed(t, o, body, seed)
			}
			s := o.stats(true)
			if !s.observed || !s.usage || s.prompt != tc.prompt || s.completion != tc.completion ||
				s.cached != tc.cached || s.cachedKnown != tc.cachedKnown ||
				s.reasoning != tc.reasoning || s.reasoningKnown != tc.reasoningKnown || s.finishReason != tc.finish {
				t.Fatalf("%s seed %d: stats = %+v", tc.fixture, seed, s)
			}
			if s.hasThink != tc.think || s.hasAnswer != tc.answer {
				t.Fatalf("%s seed %d: think %v answer %v, want %v %v", tc.fixture, seed, s.hasThink, s.hasAnswer, tc.think, tc.answer)
			}
			if tc.think && tc.answer && !(s.firstThink < s.firstAnswer) {
				t.Fatalf("%s seed %d: first think %v not before first answer %v", tc.fixture, seed, s.firstThink, s.firstAnswer)
			}
			if (tc.think || tc.answer) && (s.events == 0 || !s.hasFirstToken) {
				t.Fatalf("%s seed %d: no events timed: %+v", tc.fixture, seed, s)
			}
			if !strings.HasSuffix(tc.fixture, ".json") && !s.ended {
				t.Fatalf("%s seed %d: a whole stream did not end", tc.fixture, seed)
			}
		}
	}
}

// SGLang echoes the request's tools after the usage of a plain Responses
// reply; an agent client's tools push the usage far out of the kept tail.
func TestObserverFindsResponsesUsageBeforeEchoedTools(t *testing.T) {
	body := readFixture(t, "sglang-1f6ce4b0-responses-think.json")
	tool := `{"type":"function","name":"read_file","description":"` + strings.Repeat("Read a file. ", 40) + `","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}`
	tools := strings.TrimSuffix(strings.Repeat(tool+",", 60), ",")
	if !strings.Contains(body, `"tools":[]`) {
		t.Fatal("fixture no longer echoes an empty tools list")
	}
	body = strings.Replace(body, `"tools":[]`, `"tools":[`+tools+`]`, 1)
	if len(body)-strings.Index(body, `"usage"`) <= usageTailBytes {
		t.Fatal("the tools do not push the usage out of the tail")
	}
	o := newResponseObserver(responsesPath, 200, jsonHeader(), time.Unix(100, 0))
	feed(t, o, body, 7)
	s := o.stats(true)
	if !s.usage || s.prompt != 25 || s.completion != 37 || s.reasoning != 29 || s.finishReason != "stop" {
		t.Fatalf("stats = %+v", s)
	}
}

// A stream cut before its last event is no usage and no missing usage; one
// whose last event arrived counts even though the copy stopped short.
func TestObserverEndsAgentStreamsOnTheirLastEvent(t *testing.T) {
	for _, tc := range []struct{ path, fixture, last string }{
		{messagesPath, "sglang-1f6ce4b0-messages-think-stream.sse", "event: message_stop"},
		{responsesPath, "sglang-1f6ce4b0-responses-think-stream.sse", "event: response.completed"},
	} {
		body := readFixture(t, tc.fixture)
		cut := body[:strings.Index(body, tc.last)]
		o := newResponseObserver(tc.path, 200, sseHeader(), time.Unix(100, 0))
		feed(t, o, cut, 3)
		s := o.stats(false)
		if tc.path == responsesPath {
			// Without response.completed nothing says the reply ended.
			if s.observed || s.usage || s.ended {
				t.Fatalf("%s cut before its last event: %+v", tc.fixture, s)
			}
		} else if !s.ended || !s.usage || s.completion != 37 {
			// message_delta carried the stop reason and the usage; only
			// message_stop is missing.
			t.Fatalf("%s cut after its message_delta: %+v", tc.fixture, s)
		}
		half := body[:len(body)/3]
		o = newResponseObserver(tc.path, 200, sseHeader(), time.Unix(100, 0))
		feed(t, o, half, 3)
		if s := o.stats(false); s.observed || s.usage || s.ended {
			t.Fatalf("%s cut early: %+v", tc.fixture, s)
		}
	}
}

// The gateway asks for stream usage only where the protocol needs asking.
func TestAgentProtocolsAreNotAskedForUsage(t *testing.T) {
	for _, path := range []string{messagesPath, responsesPath} {
		r := newJSONRequest(t, path, `{"model":"m","stream":true,"messages":[]}`)
		_, injected, cleanup, err := injectUsage(r, 1<<20, true)
		cleanup()
		if err != nil || injected {
			t.Fatalf("%s: injected=%v err=%v", path, injected, err)
		}
	}
	r := newJSONRequest(t, chatPath, `{"model":"m","stream":true,"messages":[]}`)
	_, injected, cleanup, err := injectUsage(r, 1<<20, true)
	cleanup()
	if err != nil || !injected {
		t.Fatalf("chat: injected=%v err=%v", injected, err)
	}
}

func BenchmarkObserveMessagesStream(b *testing.B) {
	body := []byte(fixtureB(b, "sglang-1f6ce4b0-messages-think-stream.sse"))
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		o := newResponseObserver(messagesPath, 200, sseHeader(), time.Unix(100, 0))
		for p := body; len(p) > 0; {
			n := min(512, len(p))
			o.observe(p[:n])
			p = p[n:]
		}
		o.stats(true)
	}
}

func BenchmarkObserveResponsesStream(b *testing.B) {
	body := []byte(fixtureB(b, "sglang-1f6ce4b0-responses-think-stream.sse"))
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		o := newResponseObserver(responsesPath, 200, sseHeader(), time.Unix(100, 0))
		for p := body; len(p) > 0; {
			n := min(512, len(p))
			o.observe(p[:n])
			p = p[n:]
		}
		o.stats(true)
	}
}

func fixtureB(b *testing.B, name string) string {
	b.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		b.Fatal(err)
	}
	return string(data)
}

// Through the whole gateway: an agent client's Messages and Responses
// requests go out byte for byte as sent (nothing asked on their behalf), the
// client gets the engine's reply unchanged, and the request record and the
// token counters carry the engine's usage.
func TestGatewayRecordsAgentProtocolUsage(t *testing.T) {
	for _, tc := range []struct {
		engine, path, fixture string
		prompt, completion    uint64
		cached                int64
	}{
		{"sglang", messagesPath, "sglang-1f6ce4b0-messages-think-stream.sse", 25, 37, -1},
		{"vllm", messagesPath, "vllm-0.30-messages-cached-stream.sse", 1706, 40, 1632},
		{"vllm", responsesPath, "vllm-0.30-responses-think-stream.sse", 21, 117, 16},
		{"sglang", responsesPath, "sglang-1f6ce4b0-responses-cached-stream.sse", 1576, 38, 1536},
	} {
		t.Run(tc.engine+tc.path, func(t *testing.T) {
			reply := readFixture(t, tc.fixture)
			sent := `{"model":"omni","stream":true,"max_tokens":40,"messages":[{"role":"user","content":"hi"}]}`
			var got string
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = string(body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, reply)
			})
			p, acct, apiKey, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: tc.engine})
			defer closeUpstream()
			var records []uxrecord.Request
			var mu sync.Mutex
			p.SetRequestSink(func(r uxrecord.Request) { mu.Lock(); records = append(records, r); mu.Unlock() })
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(sent))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			p.Handler()(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != reply {
				t.Fatalf("status %d, reply changed: %v", rec.Code, rec.Body.String() != reply)
			}
			if got != sent {
				t.Fatalf("request changed on the way: %s", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(records) != 1 {
				t.Fatalf("records = %d", len(records))
			}
			r := records[0]
			if int64(r.PromptTokens) != int64(tc.prompt) || int64(r.CompletionTokens) != int64(tc.completion) || int64(r.CachedTokens) != tc.cached || r.Events == 0 {
				t.Fatalf("record = %+v", r)
			}
			_, samples := acct.snapshotAndClearDirty()
			dep := map[string]string{"deployment": itoa(depID)}
			if v, _ := sampleValue(samples, metricPromptTokens, dep); v != float64(tc.prompt) {
				t.Fatalf("prompt tokens counter = %v", v)
			}
			if v, ok := sampleValue(samples, metricUsageMissing, dep); ok && v != 0 {
				t.Fatalf("usage missing = %v", v)
			}
		})
	}
}
