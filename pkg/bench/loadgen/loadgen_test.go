package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBaseURL(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		stripped bool
	}{
		{"http://host:3000", "http://host:3000", false},
		{" http://host:3000/ ", "http://host:3000", false},
		{"http://host:3000/v1", "http://host:3000", true},
		{"http://host:3000/v1/", "http://host:3000", true},
		{"https://host/openai/v1/", "https://host/openai", true},
	} {
		got, stripped := NormalizeBaseURL(tt.in)
		if got != tt.want || stripped != tt.stripped {
			t.Errorf("NormalizeBaseURL(%q)=(%q,%v), want (%q,%v)", tt.in, got, stripped, tt.want, tt.stripped)
		}
	}
}

func TestTotalRequestsIncludesFollowUpTurns(t *testing.T) {
	requests := []Request{{FollowUps: []Message{{Role: "user"}, {Role: "user"}}}, {}, {FollowUps: []Message{{Role: "user"}}}}
	if got := TotalRequests(requests); got != 6 {
		t.Fatalf("TotalRequests = %d, want 6", got)
	}
}

func TestClientParsesStreamingUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	res, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ab" || res.RawUsage.CompletionTokens != 2 || res.Metrics.CachedTokens != 4 || !res.Metrics.CachedTokensAvailable || res.Metrics.Chunks != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestParseSSEMeasuresCompletionAtLastContent(t *testing.T) {
	started := time.Unix(100, 0)
	eventTimes := []time.Time{
		started.Add(5 * time.Millisecond),
		started.Add(25 * time.Millisecond),
		started.Add(100 * time.Millisecond),
		started.Add(110 * time.Millisecond),
	}
	clockIndex := 0
	now := func() time.Time {
		if clockIndex >= len(eventTimes) {
			t.Fatalf("SSE parser requested unexpected clock sample %d", clockIndex)
		}
		result := eventTimes[clockIndex]
		clockIndex++
		return result
	}
	stream := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":3}}\n\n" +
			"data: [DONE]\n\n",
	)
	res, err := parseSSEWithClock(stream, started, now)
	if err != nil {
		t.Fatal(err)
	}
	if clockIndex != len(eventTimes) {
		t.Fatalf("SSE parser clock samples = %d, want %d", clockIndex, len(eventTimes))
	}
	if res.Text != "ab" || res.RawUsage.PromptTokens != 4 || res.RawUsage.CompletionTokens != 3 {
		t.Fatalf("result = %+v", res)
	}
	if res.Metrics.TTFT != 5*time.Millisecond || res.Metrics.Latency != 25*time.Millisecond || res.Metrics.TPOT != 20*time.Millisecond {
		t.Fatalf("TTFT=%s latency=%s TPOT=%s", res.Metrics.TTFT, res.Metrics.Latency, res.Metrics.TPOT)
	}
	if len(res.Metrics.ITL) != 1 || res.Metrics.ITL[0] != 20*time.Millisecond {
		t.Fatalf("ITL=%v", res.Metrics.ITL)
	}
}

// SGLang and older vLLM stream reasoning_content, vLLM 0.34 streams reasoning.
func TestParseSSECountsReasoningOnlyOutput(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			started := time.Unix(100, 0)
			clockIndex := 0
			eventTimes := []time.Time{started.Add(5 * time.Millisecond), started.Add(25 * time.Millisecond), started.Add(30 * time.Millisecond)}
			now := func() time.Time {
				if clockIndex >= len(eventTimes) {
					t.Fatalf("SSE parser requested unexpected clock sample %d", clockIndex)
				}
				value := eventTimes[clockIndex]
				clockIndex++
				return value
			}
			stream := strings.NewReader(
				"data: {\"choices\":[{\"delta\":{\"" + field + "\":\"think\"}}]}\n\n" +
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":8}}\n\n" +
					"data: [DONE]\n\n",
			)
			res, err := parseSSEWithClock(stream, started, now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "" {
				t.Fatalf("reasoning must not become visible follow-up text: %q", res.Text)
			}
			if res.Metrics.StartedAt.IsZero() || res.Metrics.TTFT != 5*time.Millisecond || res.Metrics.Latency != 5*time.Millisecond || res.Metrics.OutputTokens != 8 || res.Metrics.Chunks != 1 {
				t.Fatalf("reasoning-only metrics = %+v", res.Metrics)
			}
		})
	}
}

func TestParseSSEUsesReasoningForApproximateOutputTokens(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			started := time.Unix(100, 0)
			stream := strings.NewReader("data: {\"choices\":[{\"delta\":{\"" + field + "\":\"abcdefgh\"}}]}\n\ndata: [DONE]\n\n")
			res, err := parseSSEWithClock(stream, started, func() time.Time { return started.Add(time.Millisecond) })
			if err != nil {
				t.Fatal(err)
			}
			if !res.Metrics.ApproximateTokens || res.Metrics.OutputTokens == 0 {
				t.Fatalf("reasoning-only fallback metrics = %+v", res.Metrics)
			}
		})
	}
}

func TestRunnerMultiturnAppendsRealAssistantReply(t *testing.T) {
	var bodies []Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body Request
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"real-reply"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	r := Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 1}
	got := r.Run(context.Background(), []Request{{Model: "m", Messages: []Message{{Role: "user", Content: "first"}}, FollowUps: []Message{{Role: "user", Content: "second"}}}})
	if len(got) != 2 || len(bodies) != 2 {
		t.Fatalf("results=%d bodies=%d", len(got), len(bodies))
	}
	if len(bodies[1].Messages) != 3 || bodies[1].Messages[1].Role != "assistant" || bodies[1].Messages[1].Content != "real-reply" {
		t.Fatalf("second request=%+v", bodies[1])
	}
}

func TestRunnerCancellationStopsInFlightAndQueuedRequests(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
		select {
		case <-canceled:
		default:
			close(canceled)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []Result, 1)
	go func() {
		done <- (Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 1}).Run(ctx, []Request{
			{Model: "m"}, {Model: "m"}, {Model: "m"},
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight HTTP request did not observe context cancellation")
	}
	select {
	case results := <-done:
		if len(results) != 1 || results[0].Metrics.Error == nil {
			t.Fatalf("results=%+v, want one canceled in-flight request", results)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not return after cancellation")
	}
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests=%d, queued requests continued after cancellation", got)
	}
}

func TestClientMarksApproximateTokensWhenUsageMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer srv.Close()
	res, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.ApproximateTokens || res.Metrics.InputTokens == 0 || res.Metrics.OutputTokens == 0 {
		t.Fatalf("expected approximate token fallback: %+v", res.Metrics)
	}
}

func TestClientDoesNotApproximateUsageBackedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	res, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.ApproximateTokens {
		t.Fatal("usage-backed request must remain exact")
	}
}

func TestClientParsesNonStreamingCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`))
	}))
	defer srv.Close()
	res, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.CachedTokensAvailable || res.Metrics.CachedTokens != 2 || res.Metrics.ApproximateTokens {
		t.Fatalf("unexpected metrics: %+v", res.Metrics)
	}
}

// A runner told to discard results keeps none, yet every result still reaches
// OnResult, progress still counts each one, and a follow-up turn still carries
// the previous answer: only the point-long retention goes away.
func TestRunnerDiscardResultsKeepsNoneButDeliversEach(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"answer-one"}}]}`+"\n\n"+
			`data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	var delivered int
	var progress []int
	results := Runner{
		Client: Client{BaseURL: srv.URL}, Concurrency: 1, DiscardResults: true,
		OnResult:   func(Result) { mu.Lock(); delivered++; mu.Unlock() },
		OnProgress: func(done, total int) { mu.Lock(); progress = append(progress, done*10+total); mu.Unlock() },
	}.Run(context.Background(), []Request{
		{Model: "m", Stream: true, Messages: []Message{{Role: "user", Content: "q1"}}, FollowUps: []Message{{Role: "user", Content: "q2"}}},
		{Model: "m", Stream: true, Messages: []Message{{Role: "user", Content: "q3"}}},
	})
	if results != nil {
		t.Fatalf("discarding runner returned %d results", len(results))
	}
	if delivered != 3 {
		t.Fatalf("OnResult saw %d results, want 3", delivered)
	}
	if want := []int{13, 23, 33}; fmt.Sprint(progress) != fmt.Sprint(want) {
		t.Fatalf("progress (done*10+total) = %v, want %v", progress, want)
	}
	if len(bodies) != 3 || !strings.Contains(bodies[1], `"answer-one"`) || !strings.Contains(bodies[1], `"q2"`) {
		t.Fatalf("follow-up turn body = %q, want the previous answer and the next question", bodies)
	}
}
