package report

import (
	"fmt"
	"math"
	"strings"

	"gpuworks.ai/gpuworks/pkg/bench/search"
)

func pointObservation(row pointRow) search.Observation {
	observation := search.WithEvidence(search.Observation{
		Point:      search.Point{Concurrency: row.Concurrency, ISL: row.TargetISL, OSL: row.TargetOSL, RequestRate: row.RequestRate},
		Throughput: row.OutputTput, TotalThroughput: row.TotalTput, RequestTput: row.RequestTput, SuccessRate: row.SuccessRate,
		TTFT: row.P50TTFT, AvgTTFT: row.AvgTTFT, P99TTFT: row.P99TTFT,
		TPOT: row.P50TPOT, AvgTPOT: row.AvgTPOT, P50TPOT: row.P50TPOT, P99TPOT: row.P99TPOT,
		Latency: row.P50Latency, AvgLatency: row.AvgLatency, P99Latency: row.P99Latency,
	}, row.LongTail)
	return observation
}

func recommend(run Run, rows []pointRow) string {
	doc, err := search.ParseDocument(run.Search)
	if err != nil {
		return "no recommendation: search constraints unavailable"
	}
	observations := make([]search.Observation, 0, len(rows))
	for _, row := range rows {
		if row.State == "canceled" || row.State == "failed" || (row.Evidence != nil && row.Evidence.All.Success == 0) {
			continue
		}
		observations = append(observations, pointObservation(row))
	}
	best, ok := search.BestMeasured(observations, doc.SLA())
	if !ok {
		return "no recommendation: no measured point satisfies the declared experience constraints and evidence requirements"
	}
	return fmt.Sprintf("best measured point in this run: %s; objective=%s; output throughput %.3f tok/s; interactivity %s tok/s/user (decode only); comparison warnings and declared constraints apply; no capacity upper bound", recommendedLoad(Point{Concurrency: best.Point.Concurrency, RequestRate: best.Point.RequestRate}), first(doc.Objective, "output_tps"), best.Throughput, bestInteractivity(best))
}

func bestInteractivity(observation search.Observation) string {
	if observation.Samples != nil && observation.Samples.TPOT == 0 {
		return "unknown"
	}
	if observation.P50TPOT <= 0 {
		return "unknown"
	}
	return number(1000 / observation.P50TPOT)
}

func comparisonWarnings(run Run, dataset datasetConfig) []string {
	var warnings []string
	unknownEvidence, cacheRisk, incomplete := false, false, false
	var first *pointRow
	var minOutput, maxOutput, minInput, maxInput float64
	for _, point := range run.Points {
		e := pointEvidence(point)
		if e == nil {
			unknownEvidence = true
		} else {
			cacheRisk = cacheRisk || e.Execution.CacheState == "" || e.Execution.CacheState == "unknown" || e.Execution.CachePolicy == "unknown"
			incomplete = incomplete || e.Outcomes.CompletedSessions != e.Plan.PlannedSessions || e.All.Attempted != e.Plan.PlannedRequests
			if peak := e.All.Tokens.Input; dataset.PeakInputTokens > 0 && peak.Count > 0 && peak.Max > dataset.PeakInputTokens {
				warnings = append(warnings, fmt.Sprintf("Point %d: observed peak input %d exceeds planned peak input %d tokens; inspect token sources before attributing a cause.", point.Seq, peak.Max, dataset.PeakInputTokens))
			}
			if context := e.All.Tokens.Context; dataset.RequiredContextTokens > 0 && context.Count > 0 && context.Max > dataset.RequiredContextTokens {
				warnings = append(warnings, fmt.Sprintf("Point %d: observed same-request context peak %d exceeds planned required context %d tokens. The actual service context limit is unknown; this is not proof of a server limit error.", point.Seq, context.Max, dataset.RequiredContextTokens))
			}
			if e.Plan.OutputPolicy == "ignore_eos_requested" && e.All.Tokens.Output.Count > 0 && tokenMean(e.All.Tokens.Output) != number(float64(e.Plan.OutputBudget)) {
				warnings = append(warnings, fmt.Sprintf("Point %d: requested fixed-length diagnostic policy did not produce the target mean output; ignore_eos requested does not prove endpoint compliance.", point.Seq))
			}

			if first == nil {
				first = &pointRow{Point: point, Evidence: e}
			} else {
				f := first.Evidence
				if f.Plan.OutputPolicy != e.Plan.OutputPolicy || f.Plan.OutputBudget != e.Plan.OutputBudget || f.Execution.CachePolicy != e.Execution.CachePolicy || f.Execution.RetryPolicy != e.Execution.RetryPolicy || f.Execution.SeedDerivation != e.Execution.SeedDerivation || f.Plan.TurnsPerSession != e.Plan.TurnsPerSession || f.Execution.TokenizerSource != e.Execution.TokenizerSource {
					warnings = append(warnings, fmt.Sprintf("Comparison conditions changed at point %d: output/cache/retry/seed derivation, tokenizer or turn policy differs.", point.Seq))
				}
			}
		}
		if point.ActualOSL > 0 {
			if minOutput == 0 || point.ActualOSL < minOutput {
				minOutput = point.ActualOSL
			}
			maxOutput = math.Max(maxOutput, point.ActualOSL)
		}
		if point.ActualISL > 0 {
			if minInput == 0 || point.ActualISL < minInput {
				minInput = point.ActualISL
			}
			maxInput = math.Max(maxInput, point.ActualISL)
		}
	}
	if unknownEvidence {
		warnings = append(warnings, "Comparison evidence insufficient: historical point policies, session completion and sample counts are unknown; no backfill from aggregate values.")
	}
	if cacheRisk {
		warnings = append(warnings, "Comparison evidence insufficient: cache state is unknown; later points may inherit prior cache state. A different seed is not evidence of cold or isolated cache.")
	}
	if incomplete {
		warnings = append(warnings, "Comparison conditions changed: sessions ended early or planned requests were not all attempted; request success is not session completion.")
	}
	if minOutput > 0 && maxOutput/minOutput > 1.2 {
		warnings = append(warnings, "Comparison conditions changed: observed mean output lengths differ by more than 20%; throughput differences also reflect different completed work.")
	}
	if minInput > 0 && maxInput/minInput > 1.1 {
		warnings = append(warnings, "Comparison conditions changed: observed mean input lengths differ by more than 10%.")
	}
	if dataset.OutputTokens > 0 && minOutput > 0 && minOutput < float64(dataset.OutputTokens)*.8 {
		warnings = append(warnings, "Observed output is shorter than the configured budget. A budget is an upper limit; inspect finish reasons and token sources before attributing a cause.")
	}
	if len(run.Points) > 0 {
		warnings = append(warnings, "Repeated-measurement uncertainty is not estimated; recommendation is limited to measured points and declared constraints.")
	}
	return warnings
}

func recommendationProperties(run Run, rows []pointRow) [][2]string {
	doc, err := search.ParseDocument(run.Search)
	if err != nil {
		return [][2]string{{"Constraints", "unknown: invalid stored search document"}}
	}
	out := make([][2]string, 0, len(rows))
	for _, row := range rows {
		reason := "eligible under declared constraints"
		switch {
		case row.State == "canceled" || row.State == "failed":
			reason = "excluded: point " + row.State
		case row.Evidence != nil && row.Evidence.All.Success == 0:
			reason = "excluded: no successful requests"
		default:
			o := pointObservation(row)
			violations := doc.SLA().Violations(o)
			if len(violations) > 0 {
				parts := make([]string, 0, len(violations))
				for _, violation := range violations {
					actual := number(violation.Actual)
					if violation.Reason == "evidence_unavailable" {
						actual = "unknown"
					}
					parts = append(parts, fmt.Sprintf("%s: %s; actual=%s, limit=%s", violation.Metric, violation.Reason, actual, number(violation.Limit)))
				}
				reason = "excluded: " + strings.Join(parts, "; ")
			} else if _, ok := search.BestMeasured([]search.Observation{o}, doc.SLA()); !ok {
				reason = "excluded: no positive measured work for the declared objective"
			}
		}
		out = append(out, [2]string{fmt.Sprintf("Point %d (%s)", row.Seq, pointLabel(row.Point)), reason})
	}
	return out
}

func constraintDescription(doc search.Document) string {
	var parts []string
	for _, constraint := range []struct {
		name, operator, unit string
		value                float64
	}{
		{"TTFT mean", "≤", "ms", doc.MaxAvgTTFT}, {"TTFT P99", "≤", "ms", doc.MaxP99TTFT},
		{"TPOT mean", "≤", "ms", doc.MaxAvgTPOT}, {"TPOT P99", "≤", "ms", doc.MaxP99TPOT},
		{"Last-output latency mean", "≤", "ms", doc.MaxAvgLatency}, {"Last-output latency P99", "≤", "ms", doc.MaxP99Latency},
		{"Request throughput", "≤", "req/s", doc.MaxRPS}, {"Request throughput", "≥", "req/s", doc.MinRPS},
		{"Output throughput", "≤", "tok/s", doc.MaxTPS}, {"Output throughput", "≥", "tok/s", doc.MinTPS},
		{"Decode interactivity", "≥", "tok/s/user", doc.MinInteractivity},
		{"Legacy total throughput/concurrency", "≤", "tok/s", doc.LegacyMaxPerUserTotalTPS}, {"Legacy total throughput/concurrency", "≥", "tok/s", doc.LegacyMinPerUserTotalTPS},
		{"Legacy output throughput/concurrency", "≤", "tok/s", doc.LegacyMaxPerUserOutputTPS}, {"Legacy output throughput/concurrency", "≥", "tok/s", doc.LegacyMinPerUserOutputTPS},
	} {
		if constraint.value > 0 {
			parts = append(parts, fmt.Sprintf("%s %s %s %s", constraint.name, constraint.operator, number(constraint.value), constraint.unit))
		}
	}
	if doc.MinSessionCompletion > 0 {
		parts = append(parts, "Session completion ≥ "+percentage(doc.MinSessionCompletion))
	}
	if len(parts) == 0 {
		return "none configured (no hidden experience gate)"
	}
	return strings.Join(parts, "; ")
}
