package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestSessionFailurePreservesRequestIdentityAndQuantityLedger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Messages[0].Content == "fail" {
			http.Error(w, `{"error":{"message":"overloaded"}}`, 503)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	reqs := make([]Request, 4)
	plans := make([]metrics.SessionEvidence, 4)
	for i := range reqs {
		reqs[i] = Request{Messages: []Message{{Role: "user", Content: "ok"}}, FollowUps: make([]Message, 12), MaxTokens: 220}
		for j := range reqs[i].FollowUps {
			reqs[i].FollowUps[j] = Message{Role: "user", Content: "next"}
		}
		plans[i] = metrics.SessionEvidence{SessionID: metrics.SessionID(i), PlannedTurns: 13}
	}
	reqs[3].Messages[0].Content = "fail"
	results := (Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 4}).Run(context.Background(), reqs)
	if len(results) != 40 {
		t.Fatalf("attempts=%d", len(results))
	}
	rs := make([]metrics.Request, len(results))
	for i, r := range results {
		rs[i] = r.Metrics
		if r.Metrics.SessionID == "" || r.Metrics.Turn < 1 || r.Metrics.PlannedTurns != 13 {
			t.Fatalf("missing identity: %+v", r.Metrics)
		}
		if r.Metrics.Error != nil {
			d := NewDetailRecord(r)
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if d.ErrorText == "" || d.HTTPStatus != 503 || d.ErrorStage != "http" || d.Metrics.StartedAt == nil || d.Metrics.FinishedAt == nil || !strings.Contains(string(b), "overloaded") {
				t.Fatalf("failure detail=%s", b)
			}
		} else if r.Metrics.FinishReason != "stop" {
			t.Fatalf("finish_reason=%q", r.Metrics.FinishReason)
		}
	}
	e := metrics.BuildPointEvidence(metrics.EvidencePlan{PlannedSessions: 4, PlannedRequests: 52}, plans, rs, metrics.EvidenceExecution{RetryPolicy: "none"}, false)
	if e.All.Attempted != 40 || e.All.Success != 39 || e.All.Failed != 1 || e.Outcomes.Skipped != 12 || e.Outcomes.CompletedSessions != 3 {
		t.Fatalf("ledger: all=%+v outcomes=%+v", e.All, e.Outcomes)
	}
	if e.FirstTurn.Attempted != 4 || e.FollowupTurns.Attempted != 36 || len(e.ByTurn) != 13 {
		t.Fatalf("turn grouping: first=%d follow=%d turns=%d", e.FirstTurn.Attempted, e.FollowupTurns.Attempted, len(e.ByTurn))
	}
}

func TestUsagePresenceAndLocalEstimatesStayIndependent(t *testing.T) {
	cases := []struct {
		name, usage                      string
		cache, serverInput, serverOutput bool
		input, output                    int
	}{
		{"absent", "", false, false, false, 2, 2},
		{"null", `,"usage":null`, false, false, false, 2, 2},
		{"empty", `,"usage":{}`, false, false, false, 2, 2},
		{"empty_cache", `,"usage":{"prompt_tokens_details":{}}`, false, false, false, 2, 2},
		{"null_cache", `,"usage":{"prompt_tokens_details":{"cached_tokens":null}}`, false, false, false, 2, 2},
		{"zero", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`, true, true, true, 0, 0},
		{"partial", `,"usage":{"prompt_tokens":7,"prompt_tokens_details":{"cached_tokens":0}}`, true, true, false, 7, 2},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"abcdefgh"},"finish_reason":"stop"}]`+tt.usage+`}`)
			}))
			defer srv.Close()
			req := Request{Messages: []Message{{Role: "user", Content: "abcdefgh"}}}
			out := (Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 1, CountTokens: func(s string) int { return len(s) / 4 }}).Run(context.Background(), []Request{req})[0]
			m := out.Metrics
			if m.CachedTokensAvailable != tt.cache || (m.ServerInputTokens != nil) != tt.serverInput || (m.ServerOutputTokens != nil) != tt.serverOutput || m.InputTokens != tt.input || m.OutputTokens != tt.output {
				t.Fatalf("usage result=%+v", m)
			}
			if m.LocalInputTokens == nil || *m.LocalInputTokens != 2 || m.LocalOutputTokens == nil || *m.LocalOutputTokens != 2 {
				t.Fatalf("local estimates lost: %+v", m)
			}
			d := NewDetailRecord(out)
			if tt.usage == "" && d.Usage != nil {
				t.Fatalf("absent usage=%s", d.Usage)
			}
			if tt.name == "null" && string(d.Usage) != "null" {
				t.Fatalf("null usage=%s", d.Usage)
			}
			if tt.name == "empty" && string(d.Usage) != "{}" {
				t.Fatalf("empty usage=%s", d.Usage)
			}
		})
	}
}

func TestSelectedContextOverflowIsAnInvalidUsageAttempt(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tt := range []struct {
		name, usage string
		invalid     bool
	}{
		{"both_server", fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":2}`, maxInt), true},
		{"mixed_server_local", fmt.Sprintf(`{"prompt_tokens":%d}`, maxInt), true},
		{"large_representable", fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":2}`, maxInt-2), false},
		{"explicit_zero", `{"prompt_tokens":0,"completion_tokens":0}`, false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"abcdefgh\"}}]}\n\ndata: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", tt.usage)
						return
					}
					fmt.Fprintf(w, `{"choices":[{"message":{"content":"abcdefgh"},"finish_reason":"stop"}],"usage":%s}`, tt.usage)
				}))
				defer srv.Close()
				result, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Messages: []Message{{Role: "user", Content: "small"}}, MaxTokens: 8, Stream: stream})
				if (err != nil) != tt.invalid {
					t.Fatalf("invalid=%v err=%v input=%d output=%d", tt.invalid, err, result.Metrics.InputTokens, result.Metrics.OutputTokens)
				}
				if tt.invalid && (result.Metrics.Error == nil || result.Metrics.ErrorStage != "usage" || string(result.UsageJSON) != tt.usage) {
					t.Fatalf("invalid usage lost failure or raw evidence: %+v usage=%s", result.Metrics, result.UsageJSON)
				}
			})
		}
	}
}

func TestLocalTokenizerContextOverflowFailsTheRunnerAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}]}`)
	}))
	defer srv.Close()
	results := (Runner{Client: Client{BaseURL: srv.URL}, CountTokens: func(string) int { return int(^uint(0) >> 1) }}).Run(context.Background(), []Request{{Messages: []Message{{Role: "user", Content: "small"}}}})
	if len(results) != 1 || results[0].Metrics.Error == nil || results[0].Metrics.ErrorStage != "usage" {
		t.Fatalf("runner accepted overflowing selected local counts: %+v", results)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamInterruptionRetainsPartialOutputAndUsage(t *testing.T) {
	injected := errors.New("connection reset")
	input := io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100}}\n\n"), errorReader{injected})
	start := time.Unix(100, 0)
	tick := 0
	out, err := parseSSEWithClock(input, start, func() time.Time { tick++; return start.Add(time.Duration(tick) * time.Millisecond) })
	if !errors.Is(err, injected) || out.Text != "partial" || out.Metrics.InputTokens != 100 || out.Metrics.OutputTokens == 0 || out.Metrics.FirstOutputAt.IsZero() || out.Metrics.FinishedAt.IsZero() {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if d := NewDetailRecord(out); d.ErrorText == "" || d.Metrics.TPOTMs != nil {
		t.Fatalf("interrupted detail=%+v", d)
	}
}

func TestStreamRejectsUnfinishedAndErrorEvents(t *testing.T) {
	for _, stream := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n",
		"data: {\"error\":{\"message\":\"out of memory\"}}\n\n",
		"data: invalid-json\n\n",
	} {
		if _, err := parseSSE(strings.NewReader(stream), time.Now()); err == nil {
			t.Fatalf("accepted stream %q", stream)
		}
	}
	// Some compatible servers terminate at finish_reason without a DONE marker.
	out, err := parseSSE(strings.NewReader("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n"), time.Now())
	if err != nil || out.Metrics.FinishReason != "length" || out.Metrics.OutputTokens != 0 {
		t.Fatalf("finished stream=%+v err=%v", out, err)
	}
}

func TestStreamTimingSeparatesReasoningContentAndFinish(t *testing.T) {
	start := time.Unix(100, 0)
	tick := 0
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":4,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\ndata: [DONE]\n\n"
	out, err := parseSSEWithClock(strings.NewReader(stream), start, func() time.Time { tick++; return start.Add(time.Duration(tick) * time.Millisecond) })
	if err != nil {
		t.Fatal(err)
	}
	m := out.Metrics
	if m.FirstOutputAt.Sub(start) != time.Millisecond || m.FirstContentAt.Sub(start) != 2*time.Millisecond || m.LastOutputAt.Sub(start) != 3*time.Millisecond || m.FinishedAt.Sub(start) != 5*time.Millisecond || out.Text != "ab" || m.ReasoningTokens == nil || *m.ReasoningTokens != 2 {
		t.Fatalf("timing: %+v", m)
	}
	d := NewDetailRecord(out)
	if d.Metrics.TPOTMs == nil || *d.Metrics.TPOTMs < 0.66 || *d.Metrics.TPOTMs > 0.67 || len(d.Metrics.ChunkIntervalsMs) != 2 {
		t.Fatalf("wire timing=%+v", d.Metrics)
	}
}

func TestNonstreamReasoningCountsGeneratedOutputWithoutEnteringConversation(t *testing.T) {
	for _, usage := range []string{"", `,"usage":{"prompt_tokens":10,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":5}}`} {
		t.Run(fmt.Sprintf("usage=%t", usage != ""), func(t *testing.T) {
			field := "reasoning_content"
			if usage != "" {
				field = "reasoning"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"choices":[{"message":{"content":"","%s":"abcdefgh"},"finish_reason":"length"}]`, field)
				_, _ = w.Write([]byte(usage + `}`))
			}))
			defer srv.Close()
			result := (Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 1}).Run(context.Background(), []Request{{Messages: []Message{{Role: "user", Content: "test"}}}})[0]
			direct, directErr := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Messages: []Message{{Role: "user", Content: "test"}}})
			if directErr != nil || direct.Text != "" || direct.Metrics.OutputTokens == 0 {
				t.Fatalf("direct reasoning result=%+v err=%v", direct, directErr)
			}
			want := 2
			if usage != "" {
				want = 7
			}
			if result.Text != "" || result.Metrics.OutputTokens != want || result.Metrics.LocalOutputTokens == nil || *result.Metrics.LocalOutputTokens != 2 {
				t.Fatalf("reasoning result=%+v", result)
			}
			d := NewDetailRecord(result)
			if d.Metrics.TTFTMs != nil || d.Metrics.TPOTMs != nil || d.Metrics.LatencyMs != nil || d.Metrics.CompletionMs == nil {
				t.Fatalf("nonstream timeline fabricated: %+v", d.Metrics)
			}
		})
	}
}

// vllmReasoningStream is the chunk sequence vLLM 0.34 sends with
// --reasoning-parser kimi_k3 and stream_options.include_usage: a role opener
// with empty content, reasoning under "reasoning", the answer under "content",
// a finish chunk, then a usage chunk with no choices.
const vllmReasoningStream = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"reasoning":"The user"},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"reasoning":" asks"},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"content":"391"},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"content":"."},"logprobs":null,"finish_reason":"stop","stop_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1793889000,"model":"kimi-k3.1-pro","choices":[],"usage":{"prompt_tokens":20,"total_tokens":26,"completion_tokens":6,"completion_tokens_details":{"reasoning_tokens":4}}}

data: [DONE]

`

func TestStreamTimingCountsVLLMReasoningField(t *testing.T) {
	start := time.Unix(100, 0)
	tick := 0
	clock := func() time.Time { tick++; return start.Add(time.Duration(tick) * 10 * time.Millisecond) }
	out, err := parseSSEWithClock(strings.NewReader(vllmReasoningStream), start, clock)
	if err != nil {
		t.Fatal(err)
	}
	m := out.Metrics
	// Events: opener 10ms, reasoning 20/30ms, content 40/50ms, usage 60ms, DONE 70ms.
	if m.TTFT != 20*time.Millisecond || m.FirstContentAt.Sub(start) != 40*time.Millisecond || m.LastOutputAt.Sub(start) != 50*time.Millisecond || m.Chunks != 4 {
		t.Fatalf("timing = %+v", m)
	}
	if out.Text != "391." || out.generatedText != "The user asks391." || m.OutputTokens != 6 || m.ReasoningTokens == nil || *m.ReasoningTokens != 4 {
		t.Fatalf("text=%q generated=%q metrics=%+v", out.Text, out.generatedText, m)
	}
	// TPOT spans reasoning and answer: 30ms over 5 inter-token gaps.
	if v, ok := m.TimePerOutputToken(); !ok || v != 6*time.Millisecond {
		t.Fatalf("TPOT = %v %v", v, ok)
	}
}

func TestStreamReasoningFieldNamesAreNotDoubleCounted(t *testing.T) {
	for _, delta := range []string{`{"reasoning_content":"abc"}`, `{"reasoning":"abc"}`, `{"thinking":"abc"}`, `{"reasoning_content":"abc","reasoning":"abc"}`} {
		stream := "data: {\"choices\":[{\"delta\":" + delta + "}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"d\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		out, err := parseSSE(strings.NewReader(stream), time.Now())
		if err != nil || out.generatedText != "abcd" || out.Text != "d" || out.Metrics.FirstOutputAt.IsZero() || out.Metrics.Chunks != 2 {
			t.Fatalf("delta %s: generated=%q text=%q metrics=%+v err=%v", delta, out.generatedText, out.Text, out.Metrics, err)
		}
	}
}

func TestNonstreamReasoningFieldCountsGeneratedOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","reasoning":"abcdefgh"},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()
	out, err := (Client{BaseURL: srv.URL}).Do(context.Background(), Request{Messages: []Message{{Role: "user", Content: "test"}}})
	if err != nil || out.Text != "" || out.generatedText != "abcdefgh" || out.Metrics.OutputTokens != 2 {
		t.Fatalf("result=%+v generated=%q err=%v", out, out.generatedText, err)
	}
}
