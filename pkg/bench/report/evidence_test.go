package report

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestFailedAggregateExportsUnknownTotalsAndThroughput(t *testing.T) {
	e := metrics.PointEvidence{Version: 1, All: metrics.RequestEvidence{Attempted: 2, Success: 2}}
	e.All.Tokens.Output = metrics.TokenDistribution{Count: 2, Total: 2}
	tail, _ := json.Marshal(map[string]any{"Evidence": e})
	run := Run{Points: []Point{{Seq: 1, State: "failed", Success: 2, OutputTokens: 2, LongTail: tail}}}
	row := csvPoint(t, run)
	for _, key := range []string{"successful_input_tokens", "actual_mean_isl", "total_tok_s", "output_tok_s"} {
		if row[key] != "" {
			t.Fatalf("%s=%q, want unavailable", key, row[key])
		}
	}
	if row["successful_requests"] != "2" || row["successful_output_tokens"] != "2" {
		t.Fatalf("valid request facts lost: %v", row)
	}
	for _, format := range []string{"html", "markdown"} {
		doc, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc.Body), "unknown / 2") {
			t.Fatalf("%s fabricated an input total", format)
		}
	}
}

func sampleEvidence() *metrics.PointEvidence {
	start := time.Date(2026, 10, 17, 9, 46, 34, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	return &metrics.PointEvidence{
		Version:  1,
		Plan:     metrics.EvidencePlan{PlannedSessions: 4, PlannedRequests: 52, TurnsPerSession: 13, OutputBudget: 220, OutputPolicy: "natural"},
		Outcomes: metrics.EvidenceOutcomes{Skipped: 12, CompletedSessions: 3, FailedSessions: 1},
		All: metrics.RequestEvidence{Attempted: 40, Success: 39, Failed: 1,
			TTFT:              metrics.MetricSummary{Count: 39, MeanMs: 100, P50Ms: 20, P99Ms: 55000},
			TPOT:              metrics.MetricSummary{Count: 36, MeanMs: 30, P50Ms: 25, P99Ms: 100},
			LastOutputLatency: metrics.MetricSummary{Count: 39, P50Ms: 100, P99Ms: 56000},
			Completion:        metrics.MetricSummary{Count: 40, P50Ms: 110, P99Ms: 57000},
			Tokens:            metrics.TokenEvidence{Input: metrics.TokenDistribution{Count: 39, Total: 39000, Min: 900, Max: 1200, P50: 1000, P99: 1200}, Output: metrics.TokenDistribution{Count: 39, Total: 1950, Min: 40, Max: 60, P50: 50, P99: 60}, Context: metrics.TokenDistribution{Count: 39, Max: 1250}, ServerInput: metrics.TokenDistribution{Count: 39, Total: 39000}, LocalInput: metrics.TokenDistribution{Count: 39, Total: 37050}, ServerOutput: metrics.TokenDistribution{Count: 39, Total: 1950}, LocalOutput: metrics.TokenDistribution{Count: 39, Total: 1900}, InputSources: map[string]int{"server_usage": 39}, OutputSources: map[string]int{"server_usage": 39}},
			Cache:             metrics.CacheEvidence{Reported: 36, Unknown: 4, RatioRequests: 2, InputTokens: 1000, CachedTokens: 100, SuccessfulReported: 36}, FinishReasons: map[string]int{"stop": 39}, ErrorStages: map[string]int{"http": 1}},
		FirstTurn:     metrics.RequestEvidence{Attempted: 4, Success: 3, Failed: 1, TTFT: metrics.MetricSummary{Count: 3, P50Ms: 55000, P99Ms: 60000}, Cache: metrics.CacheEvidence{Reported: 1, Unknown: 3, RatioRequests: 1, InputTokens: 100, CachedTokens: 100}},
		FollowupTurns: metrics.RequestEvidence{Attempted: 36, Success: 36, TTFT: metrics.MetricSummary{Count: 36, P50Ms: 20, P99Ms: 30}, TPOT: metrics.MetricSummary{Count: 36, P50Ms: 25, P99Ms: 100}, Cache: metrics.CacheEvidence{Reported: 35, Unknown: 1, RatioRequests: 1, InputTokens: 900, CachedTokens: 0}},
		ByTurn:        map[int]metrics.RequestEvidence{1: {Attempted: 4}, 2: {Attempted: 3}},
		Timing:        metrics.EvidenceTiming{StartedAt: &start, FinishedAt: &end, CompletedSessionLatency: metrics.MetricSummary{Count: 3, P50Ms: 60000, P99Ms: 119000}},
		Execution:     metrics.EvidenceExecution{Seed: 42, SeedDerivation: "seed + point index", CachePolicy: "unknown", CacheState: "unknown", RetryPolicy: "none", TokenizerSource: "tokenizer.json", LocalTokenCountMethod: "local_tokenizer_estimate", DetailPolicy: "disabled"},
		Sessions:      []metrics.SessionEvidence{{SessionID: "session-1", PlannedTurns: 13, Attempted: 1, Failed: 1, Skipped: 12, State: "failed", StartedAt: &start, FinishedAt: &end}},
	}
}
func evidencePoint(e *metrics.PointEvidence) Point {
	b, _ := json.Marshal(map[string]any{"Evidence": e})
	return Point{Seq: 1, State: "done", Concurrency: 1, Success: 39, Errors: 1, ActualISL: 1000, ActualOSL: 50, OutputTput: 16.25, InputTokens: 39000, OutputTokens: 1950, LongTail: b, DetailStatus: "disabled"}
}
func csvPoint(t *testing.T, run Run) map[string]string {
	t.Helper()
	doc, err := Render("csv", run)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(doc.Body))).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	out := map[string]string{}
	for i, key := range rows[0] {
		out[key] = rows[1][i]
	}
	return out
}
func TestEvidenceCountsGroupsTokensAndCacheAcrossFormats(t *testing.T) {
	e := sampleEvidence()
	run := Run{ID: 3, Name: "Agentic", Dataset: []byte(`{"kind":"agentic","mean_input_tokens":1000,"output_tokens":220,"turns":13}`), Points: []Point{evidencePoint(e)}}
	row := csvPoint(t, run)
	for key, want := range map[string]string{"sessions": "4", "planned_requests": "52", "requests": "40", "successful_requests": "39", "failed_requests": "1", "skipped_requests": "12", "completed_sessions": "3", "success_rate": "97.50%", "session_completion_rate": "75.00%", "cache_reported_requests": "36", "cache_unknown_requests": "4", "cache_report_coverage": "90.00%", "observed_cache_hit_rate": "10.00%", "first_turn_cache_ratio": "100.00%", "followup_cache_ratio": "0.00%", "ttft_samples": "39", "tpot_samples": "36", "first_turn_ttft_p50_ms": "55000.000", "followup_ttft_p50_ms": "20.000", "actual_peak_context": "1250", "actual_peak_osl": "60", "server_mean_isl": "1000.000", "local_mean_isl": "950.000", "measurement_seconds": "120.000", "completed_sessions_per_minute": "1.500", "detail_status": "disabled"} {
		if row[key] != want {
			t.Errorf("%s=%q want %q", key, row[key], want)
		}
	}
	var back metrics.PointEvidence
	if err := json.Unmarshal([]byte(row["evidence_json"]), &back); err != nil || back.All.FinishReasons["stop"] != 39 || back.Sessions[0].Skipped != 12 {
		t.Fatalf("CSV lost retained evidence: %+v %v", back, err)
	}
	for _, format := range []string{"markdown", "html"} {
		doc, err := Render(format, run)
		if err != nil {
			t.Fatal(err)
		}
		body := string(doc.Body)
		for _, want := range []string{"planned_requests", "52", "session_completion_rate", "75.00%", "90.00%", "10.00%", "first_turn", "followup_turns", "55000.000", "server_usage", "session-1", "unknown", "last output", "Stream completion", "stop", "disabled"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", format, want)
			}
		}
	}
}
func TestOldAndFutureEvidenceRemainUnknown(t *testing.T) {
	for _, raw := range []string{`{}`, `{"Evidence":{"version":2,"plan":{"planned_requests":52}}}`} {
		point := Point{Seq: 1, Concurrency: 1, Success: 39, Errors: 1, LongTail: []byte(raw)}
		row := csvPoint(t, Run{Points: []Point{point}})
		for _, key := range []string{"planned_requests", "session_completion_rate", "ttft_samples", "tpot_samples", "first_turn_ttft_p50_ms", "cache_unknown_requests", "effective_seed", "ttft_p50_ms"} {
			if row[key] != "" {
				t.Errorf("old %s fabricated %q", key, row[key])
			}
		}
		if row["success_rate"] != "97.50%" {
			t.Errorf("lost historical success rate: %s", row["success_rate"])
		}
		doc, _ := Render("html", Run{Points: []Point{point}})
		if !strings.Contains(string(doc.Body), "Historical values are not backfilled") {
			t.Fatal("unknown explanation missing")
		}
	}
}
func TestRecommendationUsesConstraintsAndExcludesCanceledOrEmptyPoints(t *testing.T) {
	e := sampleEvidence()
	point := evidencePoint(e)
	run := Run{Dataset: []byte(`{"kind":"agentic"}`), Search: []byte(`{"min_session_completion":0.99}`), Points: []Point{point}}
	doc, _ := Render("markdown", run)
	if !strings.Contains(string(doc.Body), "no recommendation:") || strings.Contains(string(doc.Body), "best measured point in this run:") {
		t.Fatal("failed session completion gate was recommended")
	}
	run.Search = []byte(`{"max_p99_ttft":100}`)
	doc, _ = Render("html", run)
	if !strings.Contains(string(doc.Body), "no recommendation:") {
		t.Fatal("TTFT threshold ignored")
	}
	e.All.TTFT.Count = 0
	point = evidencePoint(e)
	run.Points = []Point{point}
	run.Search = []byte(`{"max_p99_ttft":100000}`)
	doc, _ = Render("markdown", run)
	if !strings.Contains(string(doc.Body), "no recommendation:") {
		t.Fatal("missing timing samples passed threshold")
	}
	point.State = "canceled"
	run.Points = []Point{point}
	run.Search = nil
	doc, _ = Render("markdown", run)
	if !strings.Contains(string(doc.Body), "no recommendation:") {
		t.Fatal("canceled point was recommended")
	}
	point.State = "done"
	point.OutputTput = 0
	run.Points = []Point{point}
	doc, _ = Render("markdown", run)
	if !strings.Contains(string(doc.Body), "no recommendation:") {
		t.Fatal("zero work recommended")
	}
}
func TestComparisonWarningsAndCompleteSavedSnapshot(t *testing.T) {
	e := sampleEvidence()
	one := evidencePoint(e)
	two := one
	two.Seq = 2
	two.Concurrency = 4
	two.ActualOSL = 80
	run := Run{Dataset: []byte(`{"kind":"agentic","output_tokens":220}`), TargetSnapshot: []byte(`{"target_kind":"instance","entry":"instance","role":"decode","deployment_id":4,"deployment_name":"service","instance_uuid":"inst","model_source_id":"hf/model","num_gpus":2,"tensor_parallel":2,"node_id":3,"node_name":"worker","gpu_indexes":[0,1],"gpus":[{"index":0,"name":"A100","arch_id":"ampere","vendor":"nvidia","uuid":"gpu-abc","node_id":3,"node_name":"worker"}],"argv":["--tp","2"],"env":{"API_KEY":"secret-value"}}`), Points: []Point{one, two}}
	for _, format := range []string{"markdown", "html", "csv"} {
		doc, _ := Render(format, run)
		body := string(doc.Body)
		for _, want := range []string{"Comparison conditions changed", "cache state is unknown", "A100", "ampere", "gpu-abc", "worker", "decode", "unknown", "API_KEY"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %s", format, want)
			}
		}
		if strings.Contains(body, "secret-value") {
			t.Fatalf("%s leaked credential", format)
		}
	}
}

func TestLengthReconciliationFlagsContextAndRequestedFixedOutput(t *testing.T) {
	e := sampleEvidence()
	e.Plan.OutputPolicy = "ignore_eos_requested"
	run := Run{Dataset: []byte(`{"kind":"agentic","output_tokens":220,"peak_input_tokens":1100,"required_context_tokens":1200}`), Points: []Point{evidencePoint(e)}}
	doc, _ := Render("markdown", run)
	for _, want := range []string{"observed peak input 1200 exceeds planned peak input 1100", "same-request context peak 1250 exceeds planned required context 1200", "actual service context limit is unknown", "does not prove endpoint compliance", "output_budget", "220", "local_mean_isl", "950.000", "Finish reasons"} {
		if !strings.Contains(string(doc.Body), want) {
			t.Errorf("missing reconciliation evidence %q", want)
		}
	}
}
