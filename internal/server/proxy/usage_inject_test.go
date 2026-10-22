package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestScanBodyFactsFindsTopLevelStreamOnly(t *testing.T) {
	for _, tc := range []struct {
		body           string
		stream, option bool
		images         int32
	}{
		{`{"model":"m","stream":true}`, true, false, 0},
		{`{ "stream" : true , "model":"m"}`, true, false, 0},
		{`{"stream":false}`, false, false, 0},
		{`{"stream":"true"}`, false, false, 0},
		{`{"stream":true,"stream_options":{"include_usage":false}}`, true, true, 0},
		{`{"stream_options":null,"stream":true}`, true, true, 0},
		// Nested or quoted look-alikes are not the request's own fields.
		{`{"tools":[{"function":{"parameters":{"stream":true}}}],"stream":false}`, false, false, 0},
		{`{"stop":["stream","stream_options"],"stream":true}`, true, false, 0},
		{`{"messages":[{"content":"\"stream\":true \"stream_options\""}],"stream":false}`, false, false, 0},
		{`{"messages":[{"content":"a \\\" \\\\"}],"stream":true}`, true, false, 0},
		// Image parts count at any depth; a quoted "image_url" does not.
		{`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:x"}},{"type":"text","text":"\"image_url\""}]},{"content":[{"image_url":{"url":"y"}}]}],"stream":true}`, true, false, 2},
		{`[{"stream":true}]`, false, false, 0},
		{`not json`, false, false, 0},
	} {
		facts, err := scanBodyFacts(strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if facts.stream != tc.stream || facts.streamOptions != tc.option || facts.images != tc.images || facts.bytes != int64(len(tc.body)) {
			t.Errorf("%s: facts = %+v", tc.body, facts)
		}
	}
}

// A body far larger than the scanner's buffer, whose strings hold escaped
// quotes and backslash runs that may straddle any read boundary, is read
// right however it splits.
func TestScanBodyFactsSurvivesBufferBoundaries(t *testing.T) {
	for pad := 0; pad < 70; pad++ {
		filler := strings.Repeat("x", 64<<10-pad) + `\\\\\"` + strings.Repeat(`\\`, pad%5) + `\"stream\":true` + strings.Repeat("y", pad)
		body := `{"messages":[{"content":"` + filler + `"}],"stream":true,"n":1}`
		if !json.Valid([]byte(body)) {
			t.Fatalf("pad %d: test body is not JSON", pad)
		}
		facts, err := scanBodyFacts(strings.NewReader(body))
		if err != nil || !facts.stream || facts.streamOptions || facts.bytes != int64(len(body)) {
			t.Fatalf("pad %d: facts = %+v, err = %v", pad, facts, err)
		}
	}
}

func newJSONRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	replay, err := newReplayBodyFromReader(r.Body, 0)
	if err != nil {
		t.Fatal(err)
	}
	installReplayBody(r, replay)
	t.Cleanup(func() { _ = replay.Close() })
	return r
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInjectUsageAddsOnlyWhatItMust(t *testing.T) {
	body := "  {\n \"model\":\"m\",\"stream\":true}"
	r := newJSONRequest(t, chatPath, body)
	facts, injected, cleanup, err := injectUsage(r, 0, true)
	defer cleanup()
	if err != nil || !injected || !facts.stream {
		t.Fatalf("injected=%v facts=%+v err=%v", injected, facts, err)
	}
	got := readBody(t, r)
	// The whitespace after the opening brace goes (insertJSONField); the
	// engine reads the same JSON.
	if want := "  {\"stream_options\":{\"include_usage\":true},\"model\":\"m\",\"stream\":true}"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if r.ContentLength != int64(len(got)) {
		t.Fatalf("content length %d for a %d-byte body", r.ContentLength, len(got))
	}
	// Retries replay the same injected body.
	if again := readBody(t, r); again != got {
		t.Fatalf("replay = %q", again)
	}
	// A streamed text completion reports usage the same way (vLLM 0.30
	// honours stream_options on /v1/completions, as the fixture shows).
	completion := newJSONRequest(t, "/v1/completions", `{"model":"m","prompt":"x","stream":true}`)
	if _, injected, cleanup, err := injectUsage(completion, 0, true); err != nil || !injected {
		t.Fatalf("completion: injected=%v err=%v", injected, err)
	} else {
		cleanup()
	}
	if got := readBody(t, completion); got != `{"stream_options":{"include_usage":true},"model":"m","prompt":"x","stream":true}` {
		t.Fatalf("completion body = %q", got)
	}
	for _, tc := range []struct{ name, path, body, contentType string }{
		{"not streamed", chatPath, `{"model":"m"}`, "application/json"},
		{"client decided", chatPath, `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, "application/json"},
		{"operation without usage stream", "/v1/embeddings", `{"model":"m","stream":true}`, "application/json"},
		{"unknown operation", "/v1/responses", `{"model":"m","stream":true}`, "application/json"},
		{"not json", chatPath, `{"model":"m","stream":true}`, "text/plain"},
	} {
		r := newJSONRequest(t, tc.path, tc.body)
		r.Header.Set("Content-Type", tc.contentType)
		_, injected, cleanup, err := injectUsage(r, 0, true)
		cleanup()
		if err != nil || injected || readBody(t, r) != tc.body {
			t.Fatalf("%s: injected=%v err=%v body=%q", tc.name, injected, err, readBody(t, r))
		}
	}
}

// vllmUpstream answers chat requests the way vLLM 0.30 did when the
// fixtures were captured: with usage (and its per-request metrics) when the
// request asked for it, without otherwise, and it records what it was sent.
type vllmUpstream struct {
	t      *testing.T
	mu     sync.Mutex
	bodies []string
}

func (u *vllmUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, string(body))
	u.mu.Unlock()
	fixture := "vllm-0.30-think-plain.sse"
	if bytes.Contains(body, []byte(`"include_usage":true`)) {
		fixture = "vllm-0.30-think-usage.sse"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, event := range strings.SplitAfter(readFixture(u.t, fixture), "\n\n") {
		_, _ = io.WriteString(w, event)
		flusher.Flush()
	}
}

// Through the gateway: the client gets the stream vLLM sends a client that
// did not ask for usage, the engine is asked for usage, and the request's
// record carries who sent it, its timeline, its usage and vLLM's own
// queue/prefill/decode times. The in-flight table is empty afterwards.
func TestGatewayRecordsAStreamedVLLMRequest(t *testing.T) {
	upstream := &vllmUpstream{t: t}
	p, _, key, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	keyID, ok := p.store.ResolveAPIKey(key)
	if !ok {
		t.Fatal("test key does not resolve")
	}
	records := make(chan uxrecord.Request, 4)
	p.SetRequestSink(func(r uxrecord.Request) { records <- r })
	p.routes.rebuild()

	client := `{"model":"omni","messages":[{"role":"user","content":"What is 17 plus 25?"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(client))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if normalize(rec.Body.String()) != normalize(readFixture(t, "vllm-0.30-think-plain.sse")) {
		t.Fatalf("the client did not get the stream it asked for:\n%s", tail(rec.Body.String()))
	}
	upstream.mu.Lock()
	sent := upstream.bodies[0]
	upstream.mu.Unlock()
	if !strings.HasPrefix(sent, `{"stream_options":{"include_usage":true},`) || !strings.HasSuffix(sent, client[1:]) {
		t.Fatalf("engine was sent %q", sent)
	}
	var got uxrecord.Request
	select {
	case got = <-records:
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
	}
	if got.DeploymentID != depID || got.APIKeyID != keyID.KeyID || got.Source != uxrecord.SourceAPI || got.InstanceUUID != "omni-instance" || got.Path != chatPath {
		t.Fatalf("identity = %+v", got)
	}
	if !got.Stream || got.Status != 200 || got.Outcome != uxrecord.OutcomeOK || got.FinishReason != "stop" || got.BodyBytes != int64(len(client)) {
		t.Fatalf("request = %+v", got)
	}
	if !got.Usage || got.PromptTokens != 21 || got.CompletionTokens != 362 || got.CachedTokens != 16 || got.ReasoningTokens != 347 {
		t.Fatalf("usage = %+v", got)
	}
	if got.EnginePrefillMS != 60 || got.EngineDecodeMS != 11328 || got.EngineQueueMS != 0 {
		t.Fatalf("engine phases = queue %d prefill %d decode %d", got.EngineQueueMS, got.EnginePrefillMS, got.EngineDecodeMS)
	}
	if got.FirstThinkMS < 0 || got.FirstAnswerMS < got.FirstThinkMS || got.LastChunkMS < got.FirstAnswerMS || got.EndMS < got.LastChunkMS || got.Events < 300 || got.ThinkEvents == 0 {
		t.Fatalf("timeline = %+v", got)
	}
	if snap := p.LiveRequests(depID, 10); snap.Waiting+snap.Thinking+snap.Answering != 0 || len(snap.Requests) != 0 {
		t.Fatalf("in-flight after the reply: %+v", snap)
	}
}

// A client that asked for usage itself gets it, untouched.
func TestGatewayLeavesAClientUsageRequestAlone(t *testing.T) {
	upstream := &vllmUpstream{t: t}
	p, _, key, _, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	p.routes.rebuild()
	client := `{"model":"omni","stream":true,"stream_options":{"include_usage":true},"messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(client))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if normalize(rec.Body.String()) != normalize(readFixture(t, "vllm-0.30-think-usage.sse")) {
		t.Fatal("a client's own usage event was removed")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.bodies[0] != client {
		t.Fatalf("engine was sent %q", upstream.bodies[0])
	}
}

// A playground request is recorded as the signed-in user's, through the same
// usage injection and stripping as /v1.
func TestPlaygroundRecordsTheSignedInUser(t *testing.T) {
	upstream := &vllmUpstream{t: t}
	p, _, _, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	// The playground checks the deployment's contract, which /v1 leaves to the engine.
	dep, err := p.store.GetDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.SetModelInferenceConfig(dep.ModelID, `[{"type":"openai.chat"}]`, `["text"]`); err != nil {
		t.Fatal(err)
	}
	records := make(chan uxrecord.Request, 4)
	p.SetRequestSink(func(r uxrecord.Request) { records <- r })
	p.routes.rebuild()

	client := `{"model":"omni","messages":[{"role":"user","content":"What is 17 plus 25?"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(client))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithClaims(req.Context(), auth.Claims{UserID: 42}))
	rec := httptest.NewRecorder()
	p.PlaygroundHandler(nil)(rec, req)
	if rec.Code != http.StatusOK || normalize(rec.Body.String()) != normalize(readFixture(t, "vllm-0.30-think-plain.sse")) {
		t.Fatalf("status %d, stream changed:\n%s", rec.Code, tail(rec.Body.String()))
	}
	select {
	case got := <-records:
		if got.Source != uxrecord.SourcePlayground || got.UserID != 42 || got.APIKeyID != 0 || got.DeploymentID != depID || !got.Usage {
			t.Fatalf("record = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
	}
}

// The scanner keeps the top-level sampling parameters the client set, as
// numbers; quoted, nested, null and malformed look-alikes are not them, and
// max_completion_tokens wins over max_tokens whatever the order.
func TestScanBodyFactsReadsTheSamplingParameters(t *testing.T) {
	for _, tc := range []struct {
		body string
		want uxrecord.Sampling
	}{
		{`{"model":"m","max_tokens":256,"temperature":0.7,"top_p":0.95,"n":2}`,
			uxrecord.Sampling{MaxTokens: 256, Temperature: 0.7, TopP: 0.95, N: 2, HasMaxTokens: true, HasTemperature: true, HasTopP: true, HasN: true}},
		{"{ \"temperature\" :\t0 , \"top_p\":1e0\n}", uxrecord.Sampling{Temperature: 0, TopP: 1, HasTemperature: true, HasTopP: true}},
		{`{"max_completion_tokens":64,"max_tokens":9999}`, uxrecord.Sampling{MaxTokens: 64, HasMaxTokens: true}},
		{`{"max_tokens":9999,"max_completion_tokens":64}`, uxrecord.Sampling{MaxTokens: 64, HasMaxTokens: true}},
		{`{"temperature":-0.5}`, uxrecord.Sampling{Temperature: -0.5, HasTemperature: true}},
		// Not the request's own numbers, or not numbers at all.
		{`{"temperature":"0.7","top_p":null,"n":true,"max_tokens":1.5}`, uxrecord.Sampling{}},
		{`{"max_tokens":-1,"n":99999999999}`, uxrecord.Sampling{}},
		{`{"messages":[{"content":"\"temperature\":0.1"}],"tools":[{"parameters":{"max_tokens":5,"n":3}}]}`, uxrecord.Sampling{}},
		{`{"top_p":0.9e}`, uxrecord.Sampling{}},
		{`{"n":1`, uxrecord.Sampling{N: 1, HasN: true}}, // a body cut short still says what it said
	} {
		facts, err := scanBodyFacts(strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if facts.sampling != tc.want {
			t.Errorf("%s: sampling = %+v, want %+v", tc.body, facts.sampling, tc.want)
		}
	}
}

// Every JSON request is read, whatever the engine; only an engine the
// gateway asks for usage gets the injection.
func TestInjectUsageReadsWithoutInjecting(t *testing.T) {
	for _, path := range []string{chatPath, "/v1/completions", "/v1/embeddings"} {
		body := `{"model":"m","stream":true,"max_tokens":8,"messages":[{"content":[{"image_url":{"url":"x"}}]}]}`
		r := newJSONRequest(t, path, body)
		facts, injected, cleanup, err := injectUsage(r, 0, false)
		cleanup()
		if err != nil || injected || readBody(t, r) != body {
			t.Fatalf("%s: injected=%v err=%v", path, injected, err)
		}
		if !facts.stream || facts.images != 1 || facts.sampling.MaxTokens != 8 || facts.bytes != int64(len(body)) {
			t.Fatalf("%s: facts = %+v", path, facts)
		}
	}
}
