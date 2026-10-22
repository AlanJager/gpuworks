package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockEnv is a ToolEnv over the fake platform API. topoBarrier, when set,
// makes get_gpu_topology wait for compare_with_healthy_replica, so a serial
// loop deadlocks; compare is what compare_with_healthy_replica answers.
type mockEnv struct {
	api         *fakeAPI
	topoBarrier chan struct{}
	compare     any
	mu          sync.Mutex
	topoNode    string
}

func newMockEnv() *mockEnv { return &mockEnv{api: newFakeAPI()} }

func (m *mockEnv) API() APIReader { return m.api }
func (m *mockEnv) GPUTopology(node string) (any, error) {
	m.mu.Lock()
	m.topoNode = node
	m.mu.Unlock()
	if m.topoBarrier != nil {
		select {
		case <-m.topoBarrier:
		case <-time.After(2 * time.Second):
			return nil, errors.New("tool calls ran one after another")
		}
	}
	return map[string]any{"links": [][]string{{"X", "NV18"}, {"NV18", "X"}}}, nil
}
func (m *mockEnv) CompareWithHealthyReplica(string) (any, error) {
	if m.topoBarrier != nil {
		close(m.topoBarrier)
	}
	if m.compare == nil {
		return nil, errors.New("instance i9 not found")
	}
	return m.compare, nil
}

// llm serves canned turns and records every request.
type llm struct {
	t     *testing.T
	turns []string
	reqs  []ChatRequest
	mu    sync.Mutex
}

func (l *llm) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			l.t.Errorf("decode: %v", err)
		}
		l.mu.Lock()
		l.reqs = append(l.reqs, req)
		n := len(l.reqs)
		l.mu.Unlock()
		if n > len(l.turns) {
			http.Error(w, `{"error":{"message":"unexpected call"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(l.turns[n-1]))
	}))
}

func toolTurn(calls ...string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
}

func call(id, name, args string) string {
	raw, _ := json.Marshal(args)
	return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + string(raw) + `}}`
}

func answerTurn(text string) string {
	raw, _ := json.Marshal(text)
	return `{"choices":[{"message":{"role":"assistant","content":` + string(raw) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":50}}`
}

func TestRunExecutesToolAndFeedsResultBack(t *testing.T) {
	l := &llm{t: t, turns: []string{
		toolTurn(call("c1", "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":"{name, gpus: [.gpus[].name]}"}`)),
		answerTurn("This node has 2 H100 GPUs; a 32B model fits with tp=2."),
	}}
	srv := l.server()
	defer srv.Close()
	env := newMockEnv()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, APIKey: "k", Model: "test"}, "127.0.0.1"), env).Run(context.Background(), SystemDeployAdvice, "Can this node run a 32B model?", DefaultMaxSteps)
	if err != nil {
		t.Fatal(err)
	}
	if got := env.api.issued(); len(got) != 1 || got[0] != "/api/v1/nodes/"+uuidA || !strings.Contains(res.Answer, "H100") {
		t.Fatalf("reads %v, answer %q", got, res.Answer)
	}
	if len(l.reqs[0].Tools) != len(BuiltinTools()) {
		t.Fatalf("first call offered %d tools, want all %d", len(l.reqs[0].Tools), len(BuiltinTools()))
	}
	fed := ""
	for _, m := range l.reqs[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			fed = m.Content
		}
	}
	if fed != `{"gpus":["H100","H100"],"name":"gpu-a"}` {
		t.Fatalf("tool result fed back = %q", fed)
	}
	if res.Calls != 2 || res.Usage != (Usage{PromptTokens: 400, CompletionTokens: 60}) {
		t.Fatalf("calls %d usage %+v", res.Calls, res.Usage)
	}
	if len(res.Steps) != 1 || res.Steps[0].Tool != "call_api" || !strings.HasPrefix(res.Steps[0].Args, `{"path":"/api/v1/nodes/`) || res.Steps[0].Error != "" {
		t.Fatalf("steps = %+v", res.Steps)
	}
}

// A seed that is enough is answered in one call, with tools still offered.
func TestRunAnswersFromSeedInOneCall(t *testing.T) {
	l := &llm{t: t, turns: []string{answerTurn("root cause: OOM")}}
	srv := l.server()
	defer srv.Close()
	env := newMockEnv()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), env).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err != nil || res.Answer != "root cause: OOM" || res.Calls != 1 || len(res.Steps) != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if len(l.reqs[0].Tools) != len(BuiltinTools()) {
		t.Fatalf("offered %d tools, want %d", len(l.reqs[0].Tools), len(BuiltinTools()))
	}
	if got := env.api.issued(); len(got) != 0 {
		t.Fatalf("read the API without a tool call: %v", got)
	}
}

func TestRunWithZeroStepsOffersNoTools(t *testing.T) {
	l := &llm{t: t, turns: []string{answerTurn("ok")}}
	srv := l.server()
	defer srv.Close()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "hi", 0)
	if err != nil || res.Answer != "ok" || len(l.reqs[0].Tools) != 0 || res.Calls != 1 {
		t.Fatalf("res %+v err %v tools %d", res, err, len(l.reqs[0].Tools))
	}
}

// One turn's tool calls run concurrently: get_gpu_topology waits for
// compare_with_healthy_replica, which a serial loop would only start
// afterwards.
func TestRunExecutesOneTurnsToolCallsConcurrently(t *testing.T) {
	l := &llm{t: t, turns: []string{
		toolTurn(
			call("a", "get_gpu_topology", `{"node_uuid":"`+uuidA+`"}`),
			call("b", "compare_with_healthy_replica", `{"instance_uuid":"i1"}`),
		),
		answerTurn("NVLink between both cards"),
	}}
	srv := l.server()
	defer srv.Close()
	env := newMockEnv()
	env.topoBarrier = make(chan struct{})
	env.compare = map[string]any{"healthy_replica": "i2", "differences": []string{"--tensor-parallel-size 2 vs 1"}}
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), env).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.Steps[0].Tool != "get_gpu_topology" || res.Steps[0].Error != "" || res.Steps[1].Tool != "compare_with_healthy_replica" || res.Steps[1].Error != "" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if env.topoNode != uuidA {
		t.Fatalf("get_gpu_topology asked for node %q", env.topoNode)
	}
	// Replies keep the order the model asked in.
	var ids, contents []string
	for _, m := range l.reqs[1].Messages {
		if m.Role == "tool" {
			ids = append(ids, m.ToolCallID)
			contents = append(contents, m.Content)
		}
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("tool replies in order %v", ids)
	}
	if contents[0] != `{"links":[["X","NV18"],["NV18","X"]]}` || contents[1] != `{"differences":["--tensor-parallel-size 2 vs 1"],"healthy_replica":"i2"}` {
		t.Fatalf("tool replies = %q", contents)
	}
}

func TestRunTruncatesLargeToolResultsAndReportsToolErrors(t *testing.T) {
	l := &llm{t: t, turns: []string{
		toolTurn(
			call("a", "compare_with_healthy_replica", `{"instance_uuid":"i1"}`),
			call("b", "call_api", `{"path":"/api/v1/nope"}`),
			call("c", "drop_tables", `{}`),
			call("d", "get_gpu_topology", `{"node_uuid":`),
		),
		answerTurn("done"),
	}}
	srv := l.server()
	defer srv.Close()
	env := newMockEnv()
	env.compare = strings.Repeat("x", maxToolResultChars+500)
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), env).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err != nil {
		t.Fatal(err)
	}
	replies := map[string]string{}
	for _, m := range l.reqs[1].Messages {
		if m.Role == "tool" {
			replies[m.ToolCallID] = m.Content
		}
	}
	if len(replies["a"]) > maxToolResultChars+200 || !strings.Contains(replies["a"], "[truncated 502 more bytes; narrow the query") {
		t.Fatalf("large result not truncated: %d bytes, tail %q", len(replies["a"]), replies["a"][max(0, len(replies["a"])-150):])
	}
	if replies["b"] != "error: no GET endpoint /api/v1/nope; find one with search_api" || replies["c"] != `error: unknown tool "drop_tables"` {
		t.Fatalf("error replies = %q / %q", replies["b"], replies["c"])
	}
	if !strings.HasPrefix(replies["d"], "error: arguments are not valid JSON for this tool (") || !strings.HasSuffix(replies["d"], "); resend the call with a JSON object") {
		t.Fatalf("malformed arguments reply = %q", replies["d"])
	}
	if res.Steps[0].Error != "" || res.Steps[1].Error != "no GET endpoint /api/v1/nope; find one with search_api" || res.Steps[2].Error != `unknown tool "drop_tables"` || res.Steps[3].Error == "" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if got := env.api.issued(); len(got) != 0 {
		t.Fatalf("an unknown route was read: %v", got)
	}
}

const wrapUpNudge = "You have 2 tool rounds left. Answer now if what you have is enough."

func countNudges(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && m.Content == wrapUpNudge {
			n++
		}
	}
	return n
}

// Out of budget, the loop asks for a final answer with no tools offered. A
// two-round budget never reaches the wrap-up nudge: it is sent with two
// rounds left, after a round has been spent.
func TestRunAsksForFinalAnswerWhenBudgetRunsOut(t *testing.T) {
	topo := call("1", "get_gpu_topology", `{"node_uuid":"`+uuidA+`"}`)
	l := &llm{t: t, turns: []string{toolTurn(topo), toolTurn(topo), answerTurn("best effort")}}
	srv := l.server()
	defer srv.Close()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "seed", 2)
	if err != nil || res.Answer != "best effort" || res.Calls != 3 || len(res.Steps) != 2 {
		t.Fatalf("res %+v err %v", res, err)
	}
	last := l.reqs[2]
	final := last.Messages[len(last.Messages)-1]
	if len(last.Tools) != 0 || final.Role != "user" || final.Content != "Based on the information gathered, give your final answer now." {
		t.Fatalf("final call offered %d tools; last message %+v", len(last.Tools), final)
	}
	if n := countNudges(last.Messages); n != 0 {
		t.Fatalf("a two-round budget was nudged %d times", n)
	}
}

// With two rounds left the model is told to answer when it can; it may still
// call tools, and the nudge is not repeated.
func TestRunNudgesToWrapUpWithTwoRoundsLeft(t *testing.T) {
	topo := func(id string) string { return call(id, "get_gpu_topology", `{"node_uuid":"`+uuidA+`"}`) }
	t.Run("answers after the nudge", func(t *testing.T) {
		l := &llm{t: t, turns: []string{toolTurn(topo("1")), toolTurn(topo("2")), answerTurn("NVLink")}}
		srv := l.server()
		defer srv.Close()
		res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "seed", 4)
		if err != nil || res.Answer != "NVLink" || res.Calls != 3 || len(res.Steps) != 2 {
			t.Fatalf("res %+v err %v", res, err)
		}
		if countNudges(l.reqs[0].Messages) != 0 || countNudges(l.reqs[1].Messages) != 0 {
			t.Fatal("nudged with more than two rounds left")
		}
		third := l.reqs[2].Messages
		// The nudge follows the second round's tool reply, and the
		// conversation goes on with the tools still offered.
		if n := len(third); third[n-1].Role != "user" || third[n-1].Content != wrapUpNudge || third[n-2].Role != "tool" || third[n-2].ToolCallID != "2" {
			t.Fatalf("third call ends with %+v", third[max(0, n-2):])
		}
		if len(l.reqs[2].Tools) != len(BuiltinTools()) {
			t.Fatalf("third call offered %d tools", len(l.reqs[2].Tools))
		}
	})
	t.Run("keeps calling to the end", func(t *testing.T) {
		l := &llm{t: t, turns: []string{toolTurn(topo("1")), toolTurn(topo("2")), toolTurn(topo("3")), toolTurn(topo("4")), answerTurn("best effort")}}
		srv := l.server()
		defer srv.Close()
		res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "seed", 4)
		if err != nil || res.Answer != "best effort" || res.Calls != 5 || len(res.Steps) != 4 {
			t.Fatalf("res %+v err %v", res, err)
		}
		for i, want := range []int{0, 0, 1, 1, 1} {
			if got := countNudges(l.reqs[i].Messages); got != want {
				t.Fatalf("call %d carries %d nudges, want %d", i+1, got, want)
			}
		}
		if len(l.reqs[4].Tools) != 0 {
			t.Fatal("the final call offered tools")
		}
	})
}

func TestRunPropagatesEndpointErrorsWithUsageSoFar(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			_, _ = w.Write([]byte(toolTurn(call("1", "get_gpu_topology", `{"node_uuid":"n1"}`))))
			return
		}
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err == nil || !strings.Contains(err.Error(), "llm http 429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
	if res.Calls != 2 || res.Usage.PromptTokens != 100 || len(res.Steps) != 1 {
		t.Fatalf("partial result = %+v", res)
	}
}

func TestRunHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(ctx, "sys", "seed", DefaultMaxSteps)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestRunRejectsResponseWithoutChoices(t *testing.T) {
	l := &llm{t: t, turns: []string{`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0}}`}}
	srv := l.server()
	defer srv.Close()
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), newMockEnv()).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err == nil || !strings.Contains(err.Error(), "no choices") || res.Usage.PromptTokens != 5 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

// countingEnv records how many comparisons run at once and in all.
type countingEnv struct {
	*mockEnv
	mu            sync.Mutex
	running, peak int
	total         int
}

func (c *countingEnv) CompareWithHealthyReplica(string) (any, error) {
	c.mu.Lock()
	c.running++
	c.total++
	c.peak = max(c.peak, c.running)
	c.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	c.mu.Lock()
	c.running--
	c.mu.Unlock()
	return map[string]any{"healthy_replica": nil}, nil
}

// One answer runs at most maxToolCalls tools, at most maxParallelTools at a
// time; the calls past the budget are answered with an error the model can
// act on, not run.
func TestRunBoundsToolCallsPerAnswerAndPerRound(t *testing.T) {
	round := func(from, n int) string {
		calls := make([]string, n)
		for i := range calls {
			calls[i] = call("c"+strconv.Itoa(from+i), "compare_with_healthy_replica", `{"instance_uuid":"i1"}`)
		}
		return toolTurn(calls...)
	}
	l := &llm{t: t, turns: []string{round(0, 20), round(20, 15), answerTurn("enough")}}
	srv := l.server()
	defer srv.Close()
	env := &countingEnv{mockEnv: newMockEnv()}
	res, err := NewRunner(NewClient(Endpoint{BaseURL: srv.URL, Model: "test"}, "127.0.0.1"), env).Run(context.Background(), "sys", "seed", DefaultMaxSteps)
	if err != nil {
		t.Fatal(err)
	}
	if env.total != maxToolCalls || env.peak > maxParallelTools || env.peak < 2 {
		t.Fatalf("ran %d tools, %d at most at once; want %d, between 2 and %d", env.total, env.peak, maxToolCalls, maxParallelTools)
	}
	if len(res.Steps) != 35 {
		t.Fatalf("steps = %d", len(res.Steps))
	}
	for i, s := range res.Steps {
		if over := i >= maxToolCalls; over != strings.Contains(s.Error, "used its 30 tool calls") {
			t.Fatalf("step %d error = %q", i, s.Error)
		}
	}
	if res.Answer != "enough" {
		t.Fatalf("answer = %q", res.Answer)
	}
}

// Every caller runs a tool through Tool.Run: the agent loop and MCP get the
// same error form and the same cut.
func TestToolRunFormatsFailuresAndCutsLongAnswers(t *testing.T) {
	long := Tool{Name: "long", Call: func(context.Context, ToolEnv, json.RawMessage) (string, error) {
		return strings.Repeat("x", maxToolResultChars+500), nil
	}}
	result, errText := long.Run(context.Background(), newMockEnv(), nil)
	if errText != "" || !strings.Contains(result, "[truncated 500 more bytes") || len(result) > maxToolResultChars+200 {
		t.Fatalf("long answer: %q… (%d chars) %q", result[:20], len(result), errText)
	}
	failing := Tool{Name: "failing", Call: func(context.Context, ToolEnv, json.RawMessage) (string, error) { return "", errors.New("boom") }}
	if result, errText := failing.Run(context.Background(), newMockEnv(), nil); result != "error: boom" || errText != "boom" {
		t.Fatalf("failure: %q %q", result, errText)
	}
	for _, builtin := range BuiltinTools() {
		if found, ok := LookupTool(builtin.Name); !ok || found.Name != builtin.Name {
			t.Fatalf("LookupTool(%q) = %v %v", builtin.Name, found.Name, ok)
		}
	}
	if _, ok := LookupTool("no_such_tool"); ok {
		t.Fatal("an unknown tool was found")
	}
}
