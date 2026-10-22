package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func intPointer(v int) *int { return &v }

func TestEvidenceCacheWeightsOnlyExplicitServerUsageAndKeepsUnknown(t *testing.T) {
	rs := []Request{
		{Turn: 1, InputTokens: 100, OutputTokens: 3, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(100), CachedTokensAvailable: true, CachedTokens: 100},
		{Turn: 2, InputTokens: 900, OutputTokens: 3, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(900), CachedTokensAvailable: true, CachedTokens: 0},
		{Turn: 2, InputTokens: 10000, OutputTokens: 3, InputSource: "character_estimate", OutputSource: "character_estimate", CachedTokensAvailable: true, CachedTokens: 10000},
		{Turn: 2, Error: errors.New("failed"), ErrorStage: "http", InputTokens: 500, ServerInputTokens: intPointer(500), CachedTokensAvailable: true, CachedTokens: 0},
		{Turn: 2, Error: errors.New("unknown"), ErrorStage: "transport"},
	}
	e := BuildPointEvidence(EvidencePlan{}, nil, rs, EvidenceExecution{}, false)
	c := e.All.Cache
	if c.Reported != 4 || c.Unknown != 1 || c.SuccessfulReported != 3 || c.RatioRequests != 3 || c.InputTokens != 1500 || c.CachedTokens != 100 {
		t.Fatalf("cache=%+v", c)
	}
	// Successful explicit requests alone are weighted 100/1000 = 10%, not 50%.
	c = e.FirstTurn.Cache
	if c.Reported != 1 || c.CachedTokens != 100 {
		t.Fatalf("first-round cache=%+v", c)
	}
	c = e.FollowupTurns.Cache
	if c.Reported != 3 || c.Unknown != 1 || c.InputTokens != 1400 || c.CachedTokens != 0 {
		t.Fatalf("followup cache=%+v", c)
	}
	two := BuildPointEvidence(EvidencePlan{}, nil, rs[:2], EvidenceExecution{}, false).All.Cache
	if float64(two.CachedTokens)/float64(two.InputTokens) != 0.1 {
		t.Fatalf("weighted cache=%+v", two)
	}
}

func TestEvidenceCountsValidTimingSamplesAndPreservesTokenSources(t *testing.T) {
	start := time.Unix(100, 0)
	rs := []Request{
		{Turn: 1, StartedAt: start, FirstOutputAt: start.Add(2500 * time.Microsecond), LastOutputAt: start.Add(10 * time.Millisecond), FinishedAt: start.Add(15 * time.Millisecond), TTFT: 2500 * time.Microsecond, TPOT: 7500 * time.Microsecond, Latency: 10 * time.Millisecond, InputTokens: 100, OutputTokens: 4, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(100), ServerOutputTokens: intPointer(4), LocalInputTokens: intPointer(90), LocalOutputTokens: intPointer(3), ReasoningTokens: intPointer(2), FinishReason: "stop"},
		{Turn: 2, StartedAt: start, FirstOutputAt: start.Add(3 * time.Millisecond), LastOutputAt: start.Add(3 * time.Millisecond), FinishedAt: start.Add(20 * time.Millisecond), TTFT: 3 * time.Millisecond, Latency: 3 * time.Millisecond, InputTokens: 20, OutputTokens: 40, InputSource: "local_tokenizer_estimate", OutputSource: "character_estimate", FinishReason: "length"},
		{Turn: 2, StartedAt: start, FinishedAt: start.Add(30 * time.Millisecond), InputTokens: 10, OutputTokens: 0, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(10), ServerOutputTokens: intPointer(0)},
		{Turn: 2, StartedAt: start, FinishedAt: start.Add(40 * time.Millisecond), OutputTokens: 7, OutputSource: "character_estimate", Error: errors.New("reset"), ErrorStage: "stream"},
	}
	e := BuildPointEvidence(EvidencePlan{}, nil, rs, EvidenceExecution{}, false)
	a := e.All
	if a.TTFT.Count != 2 || a.TPOT.Count != 1 || a.LastOutputLatency.Count != 2 || a.Completion.Count != 3 || a.TPOT.P50Ms != 2.5 || a.TTFT.P50Ms != 2.5 {
		t.Fatalf("valid timings=%+v", a)
	}
	if a.Tokens.Input.Count != 3 || a.Tokens.Output.Min != 0 || a.Tokens.Output.Max != 40 || a.Tokens.ServerOutput.Count != 2 || a.Tokens.LocalInput.Total != 90 || a.Tokens.Context.Max != 104 || a.Tokens.FailedOutput.Total != 7 || a.Tokens.Reasoning.Total != 2 {
		t.Fatalf("tokens=%+v", a.Tokens)
	}
	if a.Tokens.InputSources["server_usage"] != 2 || a.FinishReasons["length"] != 1 || a.ErrorStages["stream"] != 1 {
		t.Fatalf("sources/reasons=%+v", a)
	}
	if e.Timing.StartedAt == nil || !e.Timing.StartedAt.Equal(start) || !e.Timing.FinishedAt.Equal(start.Add(40*time.Millisecond)) {
		t.Fatalf("window=%+v", e.Timing)
	}
}

func TestEvidenceCancellationReconcilesQueuedAndInFlight(t *testing.T) {
	plans := []SessionEvidence{{SessionID: "one", PlannedTurns: 3}, {SessionID: "two", PlannedTurns: 3}, {SessionID: "three", PlannedTurns: 3}}
	rs := []Request{{SessionID: "one", Turn: 1}, {SessionID: "one", Turn: 2, Canceled: true, Error: context.Canceled}, {SessionID: "two", Turn: 1, Error: errors.New("failed")}}
	e := BuildPointEvidence(EvidencePlan{PlannedRequests: 9, PlannedSessions: 3}, plans, rs, EvidenceExecution{}, true)
	if e.All.Attempted != 3 || e.All.Success != 1 || e.All.Failed != 1 || e.All.Canceled != 1 || e.Outcomes.Skipped != 2 || e.Outcomes.CanceledBeforeStart != 4 || e.Outcomes.CanceledSessions != 2 || e.Outcomes.FailedSessions != 1 {
		t.Fatalf("all=%+v outcomes=%+v", e.All, e.Outcomes)
	}
	if e.Plan.PlannedRequests != e.All.Attempted+e.Outcomes.Skipped+e.Outcomes.CanceledBeforeStart {
		t.Fatal("ledger does not close")
	}
}

func TestEvidenceBoundsSessionDetailsAfterFullAggregation(t *testing.T) {
	start := time.Unix(100, 0)
	plans := make([]SessionEvidence, MaxEvidenceSessions+1)
	requests := make([]Request, len(plans))
	for i := range plans {
		plans[i] = SessionEvidence{SessionID: SessionID(i), PlannedTurns: 1}
		requests[i] = Request{SessionID: SessionID(i), Turn: 1, StartedAt: start, FinishedAt: start.Add(time.Second)}
	}
	e := BuildPointEvidence(EvidencePlan{PlannedSessions: len(plans), PlannedRequests: len(plans)}, plans, requests, EvidenceExecution{}, false)
	if !e.SessionsTruncated || len(e.Sessions) != MaxEvidenceSessions || e.Outcomes.CompletedSessions != len(plans) || e.Timing.CompletedSessionLatency.Count != len(plans) {
		t.Fatalf("bounded result=%d/%d %+v", len(e.Sessions), len(plans), e.Outcomes)
	}
}

func TestThroughputWindowIncludesEarlierFailureAndStreamTail(t *testing.T) {
	start := time.Unix(100, 0)
	a := New(time.Second)
	a.Add(Request{StartedAt: start, FinishedAt: start.Add(time.Second), Error: errors.New("failed")})
	a.Add(Request{StartedAt: start.Add(time.Second), Latency: time.Second, FinishedAt: start.Add(4 * time.Second), OutputTokens: 12})
	s := a.Snapshot(start.Add(4 * time.Second))
	if math.Abs(s.OutputTput-3) > 1e-9 {
		t.Fatalf("throughput=%v want 12/4", s.OutputTput)
	}
}

func TestZeroOutputStillHasRequestAndInputThroughput(t *testing.T) {
	start := time.Unix(100, 0)
	a := New(time.Second)
	a.Add(Request{StartedAt: start, FinishedAt: start.Add(4 * time.Second), InputTokens: 100, OutputTokens: 0, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(100), ServerOutputTokens: intPointer(0)})
	s := a.Snapshot(start.Add(4 * time.Second))
	if s.OutputTput != 0 || s.TotalTput != 25 || s.RequestTput != .25 {
		t.Fatalf("zero-output rates: output=%v total=%v requests=%v", s.OutputTput, s.TotalTput, s.RequestTput)
	}
}

func TestEvidenceSeedWirePreservesFullInt64AsDecimalString(t *testing.T) {
	for _, tc := range []struct {
		value int64
		wire  string
	}{
		{9223372036854775807, `"seed":"9223372036854775807"`},
		{-9223372036854775808, `"seed":"-9223372036854775808"`},
		{42, `"seed":"42"`},
	} {
		raw, err := json.Marshal(EvidenceExecution{Seed: tc.value})
		if err != nil || !strings.Contains(string(raw), tc.wire) {
			t.Fatalf("seed wire=%s err=%v", raw, err)
		}
		var execution EvidenceExecution
		if err := json.Unmarshal(raw, &execution); err != nil || execution.Seed != tc.value {
			t.Fatalf("seed roundtrip=%d want=%d err=%v", execution.Seed, tc.value, err)
		}
	}
	var literal EvidenceExecution
	if err := json.Unmarshal([]byte(`{"seed":"9223372036854775807"}`), &literal); err != nil || literal.Seed != 9223372036854775807 {
		t.Fatalf("literal seed=%d err=%v", literal.Seed, err)
	}
}

func TestEvidenceRejectsUnrepresentableContextWithoutMutatingRequests(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, output := range []int{1, 2} {
		r := Request{SessionID: "owned-session", Turn: 1, InputTokens: maxInt - 1, OutputTokens: output, InputSource: "server_usage", OutputSource: "server_usage"}
		requests := []Request{r}
		e := BuildPointEvidence(EvidencePlan{PlannedSessions: 1, PlannedRequests: 1}, []SessionEvidence{{SessionID: r.SessionID, PlannedTurns: 1}}, requests, EvidenceExecution{}, false)
		if output == 1 {
			if e.All.Success != 1 || e.All.Tokens.Context.Max != maxInt {
				t.Fatalf("representable context was rejected: %+v", e)
			}
		} else if e.All.Success != 0 || e.All.Failed != 1 || e.All.Tokens.Context.Count != 0 || e.All.ErrorStages["usage"] != 1 || e.Outcomes.FailedSessions != 1 {
			t.Fatalf("invalid context fabricated success or wrapped statistics: %+v", e)
		}
		if requests[0].Error != nil || requests[0].ErrorStage != "" {
			t.Fatal("caller request was mutated")
		}
	}
}

func TestEvidenceAggregateOverflowIsUnknownInsteadOfWrapped(t *testing.T) {
	large := int(^uint(0)>>1)/2 + 1
	requests := []Request{
		{InputTokens: large, OutputTokens: 0, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(large), CachedTokens: large, CachedTokensAvailable: true},
		{InputTokens: large, OutputTokens: 0, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(large), CachedTokens: 0, CachedTokensAvailable: true},
		{InputTokens: 1, OutputTokens: 0, InputSource: "server_usage", OutputSource: "server_usage", ServerInputTokens: intPointer(1), CachedTokens: 1, CachedTokensAvailable: true},
	}
	if strconv.IntSize < 64 {
		t.Skip("requires int64 aggregate overflow")
	}
	e := BuildPointEvidence(EvidencePlan{}, nil, requests, EvidenceExecution{}, false)
	if e.All.Success != 3 || e.All.Tokens.Input.Count != 0 || e.All.Tokens.ServerInput.Count != 0 || e.All.Tokens.Context.Count != 0 {
		t.Fatalf("overflowing token aggregate was exposed as usable: %+v", e.All)
	}
	cache := e.All.Cache
	if cache.Reported != 3 || cache.Unknown != 0 || cache.SuccessfulReported != 3 || cache.RatioRequests != 0 || cache.InputTokens != 0 || cache.CachedTokens != 0 {
		t.Fatalf("cache overflow lost coverage facts or fabricated a ratio: %+v", cache)
	}
	valid := BuildPointEvidence(EvidencePlan{}, nil, requests[:1], EvidenceExecution{}, false)
	if valid.All.Tokens.Input.Count != 1 || valid.All.Tokens.Input.Total != int64(large) || valid.All.Cache.RatioRequests != 1 || valid.All.Cache.InputTokens != int64(large) {
		t.Fatalf("large representable evidence lost: %+v", valid.All)
	}
}

func TestSnapshotNeverWrapsRepresentableTokenRatesOrIndependentTotals(t *testing.T) {
	start := time.Unix(100, 0)
	for _, inputLarge := range []bool{false, true} {
		for _, n := range []int{2, 3} {
			a := New(time.Second)
			for i := 0; i < n; i++ {
				input, output := 1, int(^uint(0)>>1)/2
				if inputLarge {
					input, output = output, input
				}
				a.Add(Request{StartedAt: start, FinishedAt: start.Add(time.Second), InputTokens: input, OutputTokens: output, Chunks: 2, CachedTokensAvailable: true})
			}
			s := a.Snapshot(start.Add(time.Second))
			if s.InputTokens < 0 || s.OutputTokens < 0 || s.TotalTput < 0 {
				t.Fatalf("n=%d wrapped snapshot: %+v", n, s)
			}
			if n == 2 && s.TotalTput <= 0 {
				t.Fatalf("representable floating-point rate lost: %+v", s)
			}
			if s.Success != n || s.Errors != 0 {
				t.Fatalf("aggregation changed real outcomes: %+v", s)
			}
			if s.RequestTput != float64(n) {
				t.Fatalf("token overflow lost independently measured request throughput: %+v", s)
			}
			if s.CacheReportCoverage == nil || *s.CacheReportCoverage != 1 {
				t.Fatalf("aggregate overflow lost cache reporting facts: %+v", s)
			}
			if inputLarge && n == 3 && strconv.IntSize == 64 {
				if s.KVHitRate != nil {
					t.Fatalf("overflowing cache ratio appears known: %+v", s)
				}
			} else if s.KVHitRate == nil || *s.KVHitRate != 0 {
				t.Fatalf("explicit zero cache observation was lost: %+v", s)
			}
			if n == 3 && strconv.IntSize == 64 {
				if s.AggregationError == "" || s.Speculative != nil {
					t.Fatalf("overflow availability was lost: %+v", s)
				}
			} else if s.AggregationError != "" {
				t.Fatalf("representable aggregate rejected: %+v", s)
			}
		}
	}
}
