package benchrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/search"
	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

func TestOverflowingPointRetainsRealRequestLedgerAndFailsWithoutNumericSamples(t *testing.T) {
	large := int(^uint(0)>>1)/2 + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", large)
	}))
	defer srv.Close()
	var statuses []proto.BenchmarkStatus
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t), ID: 1, Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random","seed":42}`), Search: json.RawMessage(`{"concurrency":[1],"isl":[1],"osl":[1],"requests_per_point_multiplier":2}`)}, Hooks{Warn: func(string) {}, Report: func(st proto.BenchmarkStatus) { statuses = append(statuses, st) }})
	last := statuses[len(statuses)-1]
	if last.State != "failed" || !strings.Contains(last.Message, "token totals exceed") {
		t.Fatalf("terminal status: %+v", last)
	}
	var point proto.BenchmarkPointResult
	found := false
	for _, status := range statuses {
		if len(status.Point) == 0 {
			continue
		}
		found = true
		if len(status.Sample) != 0 || len(status.Result) != 0 {
			t.Fatal("invalid aggregate published numeric sample/result")
		}
		if err := json.Unmarshal(status.Point, &point); err != nil {
			t.Fatal(err)
		}
	}
	if !found || point.State != "failed" || point.Success != 2 || point.Errors != 0 {
		t.Fatalf("point changed real request outcomes: %+v", point)
	}
	var tail LongTailDoc
	if err := json.Unmarshal(point.LongTail, &tail); err != nil {
		t.Fatal(err)
	}
	if tail.Evidence == nil || tail.Evidence.All.Attempted != 2 || tail.Evidence.All.Success != 2 || tail.Evidence.All.Tokens.Input.Count != 0 {
		t.Fatalf("bad retained evidence: %+v", tail.Evidence)
	}
}

func TestPointEvidencePersistsWithoutOptionalDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req loadgen.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Messages[0].Content == "fail" {
			http.Error(w, "overloaded", 503)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"short\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	dc := dataset.Config{Kind: "imported", Seed: 42, Samples: make([]dataset.Sample, 4)}
	for i := range dc.Samples {
		dc.Samples[i] = dataset.Sample{Messages: []dataset.Message{{Role: "user", Content: "ok"}}, FollowUps: make([]dataset.Message, 12), OutputTokens: 220}
		for j := range dc.Samples[i].FollowUps {
			dc.Samples[i].FollowUps[j] = dataset.Message{Role: "user", Content: "next"}
		}
	}
	dc.Samples[3].Messages[0].Content = "fail"
	raw, _ := json.Marshal(dc)
	rec := &recorder{}
	hooks := rec.hooks()
	hooks.DetailPath = nil
	hooks.Result = nil
	Execute(context.Background(), Run{ID: 20, Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: raw, Search: json.RawMessage(`{"concurrency":[1],"isl":[100],"osl":[220],"requests_per_point_multiplier":4}`)}, hooks)
	points := rec.points(t)
	if len(points) != 1 {
		t.Fatalf("points=%d state=%s", len(points), rec.last().State)
	}
	var tail LongTailDoc
	if err := json.Unmarshal(points[0].LongTail, &tail); err != nil {
		t.Fatal(err)
	}
	e := tail.Evidence
	if e == nil || e.Plan.PlannedRequests != 52 || e.All.Attempted != 40 || e.All.Success != 39 || e.All.Failed != 1 || e.Outcomes.Skipped != 12 || e.Outcomes.CompletedSessions != 3 {
		t.Fatalf("evidence=%+v", e)
	}
	if e.Plan.OutputBudget != 220 || e.Plan.OutputPolicy != "natural" || e.All.Tokens.Output.Max != 50 || e.All.FinishReasons["stop"] != 39 || e.Execution.RetryPolicy != "none" || e.Execution.CacheState != "unknown" || e.Execution.CachePolicy != "unknown" {
		t.Fatalf("execution/short output=%+v", e)
	}
	if e.All.TPOT.Count != 0 || e.All.TTFT.Count != 39 || e.FirstTurn.TTFT.Count != 3 || e.FollowupTurns.TTFT.Count != 36 {
		t.Fatalf("timing samples=%+v", e.All)
	}
	if points[0].DetailPath != "" {
		t.Fatalf("optional detail path=%q", points[0].DetailPath)
	}
	if p := rec.last(); p.State != "done" || p.PointIndex != 1 {
		t.Fatalf("terminal=%s", p.State)
	}
}

func TestRecordedSeedReproducesSamplesAndKeepsExistingCumulativeRule(t *testing.T) {
	tok, err := tokenizer.Load(strings.NewReader(`{"model":{"vocab":{"a":0,"b":1,"c":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	e := &execution{dc: dataset.Config{Kind: "agentic", Seed: 42, MeanInputTokens: 10000, Turns: 2}, tok: tok}
	point := search.Point{}
	expected := []int64{42, 42 + 10000019, 42 + 30000057}
	for i, want := range expected {
		dc := e.pointDataset(i, &point)
		reqs, err := e.requests(dc, 2)
		if err != nil {
			t.Fatal(err)
		}
		ev := e.pointEvidence(dc, reqs, nil, false)
		if ev.Execution.Seed != want || ev.Execution.SeedDerivation != "cumulative_stride_v1" || ev.Execution.LocalTokenCountMethod != "local_tokenizer_estimate" {
			t.Fatalf("point%d execution=%+v", i, ev.Execution)
		}
		arrival := e.runner(search.Point{RequestRate: 10}, dc.Seed).Arrival
		if arrival.Seed != want {
			t.Fatalf("point%d arrival seed=%d, want %d", i, arrival.Seed, want)
		}
		generated, err := e.requests(dc, 2)
		if err != nil {
			t.Fatal(err)
		}
		if generated[0].Messages[0].Content != reqs[0].Messages[0].Content {
			t.Fatal("recorded seed did not reproduce samples")
		}
	}
}

func TestCancellationDuringHTTPPreservesAttemptAndUnstartedCounts(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recorder{}
	done := make(chan struct{})
	go func() {
		Execute(ctx, Run{TokenizerRef: testTokenizerRef(t), ID: 21, Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"isl":[8],"osl":[3],"requests_per_point_multiplier":3}`)}, rec.hooks())
		close(done)
	}()
	<-started
	cancel()
	<-done
	points := rec.points(t)
	if len(points) != 1 || points[0].State != "canceled" {
		t.Fatalf("canceled points = %+v", points)
	}
	var tail LongTailDoc
	if err := json.Unmarshal(points[0].LongTail, &tail); err != nil {
		t.Fatal(err)
	}
	e := tail.Evidence
	if e.All.Attempted != 1 || e.All.Canceled != 1 || e.All.Failed != 0 || e.Outcomes.CanceledBeforeStart != 2 || e.Outcomes.CanceledSessions != 3 || e.Timing.StartedAt == nil || e.Timing.FinishedAt == nil {
		t.Fatalf("all=%+v outcomes=%+v timing=%+v", e.All, e.Outcomes, e.Timing)
	}
}

func TestEvidenceRecordsSuccessfulWarmupAndResolvedRandomSeed(t *testing.T) {
	endpoint := &fakeEndpoint{}
	srv := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t), ID: 22, Model: "m", Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":1,"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"warmup_requests":2}`)}, rec.hooks())
	points := rec.points(t)
	if len(points) != 1 {
		t.Fatalf("points=%d", len(points))
	}
	var tail LongTailDoc
	if err := json.Unmarshal(points[0].LongTail, &tail); err != nil {
		t.Fatal(err)
	}
	e := tail.Evidence
	if e.Execution.Seed == 0 || e.Execution.WarmupRequests != 2 || e.Execution.WarmupSucceeded != 2 || e.All.Attempted != 1 || e.Plan.PlannedRequests != 1 {
		t.Fatalf("warmup/seed=%+v all=%+v", e.Execution, e.All)
	}
}

func TestRecordedSeedMustBeArrivalSeed(t *testing.T) {
	for _, seed := range []int64{0, 42, -42} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var sent []time.Time
				client := &http.Client{Transport: seedTransportFunc(func(r *http.Request) (*http.Response, error) {
					defer r.Body.Close()
					var req loadgen.Request
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						return nil, err
					}
					if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || req.Model != "m" || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
						return nil, fmt.Errorf("unexpected completion request: %s %s %+v", r.Method, r.URL.Path, req)
					}
					mu.Lock()
					sent = append(sent, time.Now())
					mu.Unlock()
					body := "data: {\"choices\":[{\"delta\":{\"content\":\"short\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
					return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				rec := &recorder{}
				e := &execution{
					run:   Run{ID: 24, Model: "m", Client: loadgen.Client{BaseURL: "http://endpoint.test", HTTP: client}},
					hooks: rec.hooks(), dc: dataset.Config{Kind: "random", Seed: seed},
					doc: search.Document{PointDurationSeconds: 10, Burstiness: 0.7},
				}
				point := search.Point{Concurrency: 1, ISL: 8, OSL: 3, RequestRate: 2}
				if _, err := e.point(t.Context(), 0, &point, 1); err != nil {
					t.Fatal(err)
				}
				points := rec.points(t)
				if len(points) != 1 {
					t.Fatalf("point count=%d", len(points))
				}
				if points[0].Success != 20 || points[0].Errors != 0 {
					t.Fatalf("success=%d errors=%d, want 20 successful requests", points[0].Success, points[0].Errors)
				}
				var tail LongTailDoc
				if err := json.Unmarshal(points[0].LongTail, &tail); err != nil {
					t.Fatal(err)
				}
				if tail.Evidence == nil || tail.Evidence.Execution.Seed == 0 {
					t.Fatalf("missing resolved seed: %+v", tail.Evidence)
				}
				if seed != 0 && tail.Evidence.Execution.Seed != seed {
					t.Fatalf("configured seed=%d, recorded seed=%d", seed, tail.Evidence.Execution.Seed)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(sent) != 20 {
					t.Fatalf("sent requests=%d, want 20", len(sent))
				}
				arrival := loadgen.Arrival{Rate: point.RequestRate, Burstiness: e.doc.EffectiveBurstiness(), Seed: tail.Evidence.Execution.Seed}
				for i, want := range arrival.Gaps(len(sent)) {
					if got := sent[i+1].Sub(sent[i]); got != want {
						t.Fatalf("arrival %d: actual gap=%v, recorded seed %d reproduces %v", i+1, got, tail.Evidence.Execution.Seed, want)
					}
				}
			})
		})
	}
}

type seedTransportFunc func(*http.Request) (*http.Response, error)

func (f seedTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
