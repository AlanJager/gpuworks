package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// scriptedLLM is an OpenAI-compatible endpoint that plays fixed turns and
// records each request, so a test can see what the server sent the model.
type scriptedLLM struct {
	mu    sync.Mutex
	turns []string
	reqs  []aihelper.ChatRequest
	auth  []string
}

func (s *scriptedLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req aihelper.ChatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	n := len(s.reqs)
	s.mu.Unlock()
	if r.URL.Path != "/chat/completions" || n > len(s.turns) {
		http.Error(w, "unexpected", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(s.turns[n-1]))
}

func toolCallTurn(name, args string) string {
	raw, _ := json.Marshal(args)
	return `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"` + name + `","arguments":` + string(raw) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`
}

const finalTurn = `{"choices":[{"message":{"role":"assistant","content":"diagnosis"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`

func (f *aiFixture) configureLLM(t *testing.T, llm *scriptedLLM) {
	t.Helper()
	srv := httptest.NewServer(llm)
	t.Cleanup(srv.Close)
	box, err := auth.NewSecretbox("ai-ask-test-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.EncryptAIKey(box, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateAIConfig(&store.AIConfig{Name: "t", BaseURL: srv.URL, APIKey: key, Model: "m", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	f.ai.box = box
	f.ai.outboundAllowPrivateHosts = []string{"127.0.0.1"}
}

func toolReply(req aihelper.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return m.Content
		}
	}
	return ""
}

// recordingAPI is the platform API as a test asker reads it: it answers the
// reads it was given and records every one, so a test sees that the tools
// read through the request's reader and nothing else.
type recordingAPI struct {
	answers map[string]string
	reads   []string
}

func (r *recordingAPI) Get(_ context.Context, path string, query url.Values) (int, []byte, error) {
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	r.reads = append(r.reads, target)
	if body, ok := r.answers[path]; ok {
		return 200, []byte(body), nil
	}
	return 404, []byte(`{"error":"not found"}`), nil
}

func (r *recordingAPI) Routes() []string {
	out := make([]string, 0, len(r.answers))
	for path := range r.answers {
		out = append(out, path)
	}
	return out
}

func (r *recordingAPI) Route(path string) string {
	if _, ok := r.answers[path]; ok {
		return path
	}
	return ""
}

// The whole error landing against real stores: the seed carries the logs,
// and when the model searches further back it reads the log search API as
// the asker, through the request's reader.
func TestAskErrorSeedsContextAndRunsToolsOnRealStores(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_error")
	in := f.instance(t, "ai-ask-failed", proto.InstFailed, "engine exited with code 1", []string{"/m"})
	base := time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := f.logs.Append([]logstore.Entry{
		{Instance: in.UUID, Level: logstore.LevelWarn, Source: logstore.SourceInstance, TimeMS: base, Line: "NCCL WARN socket timeout to peer"},
		{Instance: in.UUID, Level: logstore.LevelInfo, Source: logstore.SourceInstance, TimeMS: time.Now().UnixMilli(), Line: "shutting down"},
	}); err != nil {
		t.Fatal(err)
	}
	llm := &scriptedLLM{turns: []string{toolCallTurn("call_api", `{"path":"/api/v1/logs/search","query":{"instances":"ai-ask-failed","q":"NCCL"}}`), finalTurn}}
	api := &recordingAPI{answers: map[string]string{
		"/api/v1/logs/search": `{"items":[{"id":1,"instance":"ai-ask-failed","node":"n1","level":"warn","source":"instance","time_ms":1791602099115,"line":"NCCL WARN socket timeout to peer"}],"truncated":false}`,
	}}
	f.configureLLM(t, llm)
	if !f.ai.Available() {
		t.Fatal("configured endpoint not reported available")
	}

	res, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "", Locale: "zh", API: api})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "diagnosis" || res.Calls != 2 || len(res.Steps) != 1 || res.Usage.PromptTokens != 30 {
		t.Fatalf("res = %+v", res)
	}
	first := llm.reqs[0]
	if llm.auth[0] != "Bearer sk-test" || first.Model != "m" || len(first.Tools) == 0 {
		t.Fatalf("auth %q model %q tools %d", llm.auth[0], first.Model, len(first.Tools))
	}
	if !strings.Contains(first.Messages[0].Content, "Simplified Chinese") || !strings.Contains(first.Messages[1].Content, "shutting down") {
		t.Fatalf("system/user = %q / %q", first.Messages[0].Content, first.Messages[1].Content)
	}
	// Every landing says how to read the platform and what time it is.
	if system := first.Messages[0].Content; !strings.Contains(system, "search_api") || !strings.Contains(system, "now is "+time.Now().UTC().Format("2006-01-02")) {
		t.Fatalf("system prompt lacks the platform reading: %s", system)
	}
	if !reflect.DeepEqual(api.reads, []string{"/api/v1/logs/search?instances=ai-ask-failed&q=NCCL"}) {
		t.Fatalf("reads = %v", api.reads)
	}
	if reply := toolReply(llm.reqs[1]); !strings.Contains(reply, "NCCL WARN socket timeout") || !strings.Contains(reply, "2026-10-10T03:14:59Z") {
		t.Fatalf("call_api reply = %s", reply)
	}
}

func TestAskModelSeedsMetaAndFit(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_model")
	m, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-32B", Name: "Qwen3-32B", Arch: "Qwen3ForCausalLM", WeightBytes: 61 << 30, Dtype: "bfloat16"})
	if err != nil {
		t.Fatal(err)
	}
	fitPath := "/api/v1/ai/models/" + jsonUint(m.ID) + "/fit"
	llm := &scriptedLLM{turns: []string{toolCallTurn("call_api", `{"path":"`+fitPath+`"}`), finalTurn}}
	f.configureLLM(t, llm)
	fit, err := f.ai.ModelFit(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	fitJSON, _ := json.Marshal(fit)
	api := &recordingAPI{answers: map[string]string{fitPath: string(fitJSON)}}
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskModelFit, Target: aihelper.Target{ModelID: m.ID}, Question: "Which card?", Locale: "en", API: api}); err != nil {
		t.Fatal(err)
	}
	user := llm.reqs[0].Messages[1].Content
	if !strings.HasPrefix(user, "Question: Which card?") || !strings.Contains(user, `"name":"Qwen3-32B"`) || !strings.Contains(user, "No online node reports an accelerator") {
		t.Fatalf("model seed = %s", user)
	}
	if reply := toolReply(llm.reqs[1]); !strings.Contains(reply, `65498251264`) {
		t.Fatalf("call_api fit reply = %s", reply)
	}
	if !reflect.DeepEqual(api.reads, []string{fitPath}) {
		t.Fatalf("reads = %v", api.reads)
	}
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskModelFit, Target: aihelper.Target{ModelID: 9999}, Question: "", Locale: "en"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown model err = %v", err)
	}
}

func jsonUint(v uint) string { raw, _ := json.Marshal(v); return string(raw) }

func TestAskWithoutConfigReportsNoAI(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_unconfigured")
	if f.ai.Available() {
		t.Fatal("no endpoint configured, yet available")
	}
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskAsk, Target: aihelper.Target{}, Question: "hi", Locale: "en"}); !errors.Is(err, errNoAI) {
		t.Fatalf("err = %v", err)
	}
}

// The deploy landing has no subject to seed: the question goes as is.
func TestAskDeployPassesQuestionThrough(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_deploy")
	llm := &scriptedLLM{turns: []string{finalTurn}}
	f.configureLLM(t, llm)
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskAsk, Target: aihelper.Target{}, Question: "TP for 70B?", Locale: "en"}); err != nil {
		t.Fatal(err)
	}
	if got := llm.reqs[0].Messages[1].Content; got != "TP for 70B?" {
		t.Fatalf("user = %q", got)
	}
}

// sseTurn renders one streamed completion: the given data frames, then usage
// and [DONE].
func sseTurn(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	return b.String()
}

func collect(t *testing.T, events <-chan aihelper.Event) []aihelper.Event {
	t.Helper()
	var out []aihelper.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("stream did not close")
		}
	}
}

func TestAskStreamEmitsStepsTextThenDone(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_stream")
	in := f.instance(t, "ai-stream-failed", proto.InstFailed, "engine exited", nil)
	llm := &scriptedLLM{turns: []string{
		sseTurn(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"get_instance_events","arguments":"{\"instance_uuid\":\"ai-stream-failed\"}"}}]},"finish_reason":"tool_calls"}]}`),
		sseTurn(`{"choices":[{"delta":{"content":"Root "}}]}`, `{"choices":[{"delta":{"content":"cause"},"finish_reason":"stop"}]}`),
	}}
	f.configureLLM(t, llm)
	events, err := f.ai.AskStream(context.Background(), aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "", Locale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var text string
	got := collect(t, events)
	for _, ev := range got {
		kinds = append(kinds, string(ev.Kind))
		if ev.Kind == aihelper.EventText {
			text += ev.Delta
		}
	}
	if strings.Join(kinds, ",") != "step,step,text,text,done" || text != "Root cause" {
		t.Fatalf("events %v text %q", kinds, text)
	}
	if !got[0].Running || got[1].Running || got[1].Step.Tool != "get_instance_events" {
		t.Fatalf("step events = %+v / %+v", got[0], got[1])
	}
	done := got[len(got)-1].Result
	if done.Answer != "Root cause" || done.Calls != 2 || done.Usage.PromptTokens != 14 || done.FirstTextMS <= 0 {
		t.Fatalf("done = %+v", done)
	}
	if !llm.reqs[0].Stream {
		t.Fatal("the endpoint was not asked to stream")
	}
}

func TestAskStreamFailsEarlyOrEndsWithErrorEvent(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_stream_errors")
	if _, err := f.ai.AskStream(context.Background(), aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: "nope"}, Question: "", Locale: "en"}); !errors.Is(err, errNoAI) {
		t.Fatalf("unconfigured err = %v", err)
	}
	llm := &scriptedLLM{turns: []string{"data: {\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n"}}
	f.configureLLM(t, llm)
	if _, err := f.ai.AskStream(context.Background(), aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: "nope"}, Question: "", Locale: "en"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown instance err = %v", err)
	}
	in := f.instance(t, "ai-stream-cut", proto.InstFailed, "x", nil)
	events, err := f.ai.AskStream(context.Background(), aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "", Locale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, events)
	last := got[len(got)-1]
	if last.Kind != aihelper.EventError || last.Err == nil || !strings.Contains(last.Err.Error(), "ended before") || last.Result.Calls != 1 {
		t.Fatalf("last event = %+v", last)
	}
}

// A reader that goes away stops the run: the producer never blocks on a
// channel nobody drains.
func TestAskStreamStopsWhenTheReaderLeaves(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_stream_cancel")
	in := f.instance(t, "ai-stream-cancel", proto.InstFailed, "x", nil)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 200; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	box, _ := auth.NewSecretbox("ai-ask-test-master-secret")
	key, _ := store.EncryptAIKey(box, "k")
	if _, err := f.st.CreateAIConfig(&store.AIConfig{Name: "t", BaseURL: srv.URL, APIKey: key, Model: "m", IsDefault: true}); err != nil {
		t.Fatal(err)
	}
	f.ai.box = box
	f.ai.outboundAllowPrivateHosts = []string{"127.0.0.1"}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := f.ai.AskStream(ctx, aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "", Locale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	<-events // the stream is flowing; now nobody reads
	cancel()
	select {
	case <-closed(events):
	case <-time.After(10 * time.Second):
		t.Fatal("producer kept running after the reader left")
	}
}

// closed drains events in the background and reports when the channel closes.
func closed(events <-chan aihelper.Event) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range events {
		}
		close(done)
	}()
	return done
}
