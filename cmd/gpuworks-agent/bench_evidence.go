package main

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func printBenchEvidence(raw json.RawMessage) {
	e := benchEvidence(raw)
	if e == nil {
		fmt.Println("request/session evidence=unknown")
		return
	}
	fmt.Printf("planned=%d sent=%d success=%d failed=%d canceled=%d skipped=%d canceled_before_start=%d complete_sessions=%d/%d retries=%d cache_reported=%d/%d cache_unknown=%d first_ttft_n=%d followup_ttft_n=%d tpot_n=%d cache_state=%s\n",
		e.Plan.PlannedRequests, e.All.Attempted, e.All.Success, e.All.Failed, e.All.Canceled,
		e.Outcomes.Skipped, e.Outcomes.CanceledBeforeStart, e.Outcomes.CompletedSessions, e.Plan.PlannedSessions,
		e.Outcomes.RetryCount, e.All.Cache.Reported, e.All.Attempted, e.All.Cache.Unknown,
		e.FirstTurn.TTFT.Count, e.FollowupTurns.TTFT.Count, e.All.TPOT.Count, e.Execution.CacheState)
}

func benchEvidence(raw json.RawMessage) *metrics.PointEvidence {
	var tail benchrun.LongTailDoc
	if json.Unmarshal(raw, &tail) != nil || tail.Evidence == nil || tail.Evidence.Version != 1 {
		return nil
	}
	return tail.Evidence
}

func benchPointThroughput(point proto.BenchmarkPointResult) string {
	state := ""
	if point.State != "" {
		state = "state=" + point.State + " "
	}
	e := benchEvidence(point.LongTail)
	if (e != nil && e.All.Success > 0 && (e.All.Tokens.Input.Count == 0 || e.All.Tokens.Output.Count == 0)) || (point.State == "failed" && e == nil) {
		return state + "output_tok/s=unknown total_tok/s=unknown"
	}
	return state + fmt.Sprintf("output_tok/s=%.2f total_tok/s=%.2f", point.OutputTput, point.TotalTput)
}

func benchPointTiming(point proto.BenchmarkPointResult) string {
	evidence := benchEvidence(point.LongTail)
	if evidence == nil {
		// Keep the historical CLI shape when the run has no supported evidence.
		return fmt.Sprintf("interactivity_tok/s/user=%.2f ttft_p50=%.1fms ttft_p99=%.1fms tpot_p50=%.2fms e2el_p50=%.1fms e2el_p99=%.1fms", metrics.Interactivity(point.P50TPOT), point.P50TTFT, point.P99TTFT, point.P50TPOT, point.P50Latency, point.P99Latency)
	}
	group := evidence.All
	interactivity := "unknown"
	if group.TPOT.Count > 0 && group.TPOT.P50Ms > 0 {
		interactivity = fmt.Sprintf("%.2f", metrics.Interactivity(group.TPOT.P50Ms))
	}
	return fmt.Sprintf("interactivity_tok/s/user=%s ttft_p50=%s ttft_p99=%s ttft_n=%d tpot_p50=%s tpot_p99=%s tpot_n=%d last_output_p50=%s last_output_p99=%s last_output_n=%d completion_p50=%s completion_p99=%s completion_n=%d",
		interactivity, benchMetricValue(group.TTFT, false, 1), benchMetricValue(group.TTFT, true, 1), group.TTFT.Count,
		benchMetricValue(group.TPOT, false, 2), benchMetricValue(group.TPOT, true, 2), group.TPOT.Count,
		benchMetricValue(group.LastOutputLatency, false, 1), benchMetricValue(group.LastOutputLatency, true, 1), group.LastOutputLatency.Count,
		benchMetricValue(group.Completion, false, 1), benchMetricValue(group.Completion, true, 1), group.Completion.Count)
}

func benchMetricValue(summary metrics.MetricSummary, p99 bool, precision int) string {
	if summary.Count <= 0 {
		return "unknown"
	}
	value := summary.P50Ms
	if p99 {
		value = summary.P99Ms
	}
	return fmt.Sprintf("%.*fms", precision, value)
}
