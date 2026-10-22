package metrics

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const MaxEvidenceSessions = 200

// BuildPointEvidence summarizes all measured attempts before applying the
// bounded session-detail limit. plans contains identities and planned turns,
// not request bodies. The current execution policy makes exactly one attempt.
func BuildPointEvidence(plan EvidencePlan, plans []SessionEvidence, requests []Request, execution EvidenceExecution, canceled bool) *PointEvidence {
	// Callers may supply metrics directly rather than through loadgen. Apply
	// the same validity boundary without mutating their request observations.
	requests = append([]Request(nil), requests...)
	for i := range requests {
		if requests[i].Error == nil {
			if err := requests[i].TokenCountError(); err != nil {
				requests[i].Error, requests[i].ErrorStage = err, "usage"
			}
		}
	}
	e := &PointEvidence{Version: 1, Plan: plan, Execution: execution, ByTurn: map[int]RequestEvidence{}}
	var first, follow []Request
	turns := map[int][]Request{}
	sessions := append([]SessionEvidence(nil), plans...)
	indices := map[string]int{}
	for i := range sessions {
		indices[sessions[i].SessionID] = i
	}
	for _, r := range requests {
		if r.Turn == 1 {
			first = append(first, r)
		} else if r.Turn > 1 {
			follow = append(follow, r)
		}
		if r.Turn > 0 {
			turns[r.Turn] = append(turns[r.Turn], r)
		}
		if !r.StartedAt.IsZero() && (e.Timing.StartedAt == nil || r.StartedAt.Before(*e.Timing.StartedAt)) {
			e.Timing.StartedAt = timePointer(r.StartedAt)
		}
		finish := requestFinish(r)
		if !finish.IsZero() && (e.Timing.FinishedAt == nil || finish.After(*e.Timing.FinishedAt)) {
			e.Timing.FinishedAt = timePointer(finish)
		}
		if i, ok := indices[r.SessionID]; ok {
			s := &sessions[i]
			s.Attempted++
			if r.Canceled {
				s.Canceled++
			} else if r.Error != nil {
				s.Failed++
			} else {
				s.Success++
			}
			if !r.StartedAt.IsZero() && (s.StartedAt == nil || r.StartedAt.Before(*s.StartedAt)) {
				s.StartedAt = timePointer(r.StartedAt)
			}
			if !finish.IsZero() && (s.FinishedAt == nil || finish.After(*s.FinishedAt)) {
				s.FinishedAt = timePointer(finish)
			}
		}
	}
	var sessionDurations []float64
	for i := range sessions {
		s := &sessions[i]
		remaining := max(0, s.PlannedTurns-s.Attempted)
		switch {
		case s.Success == s.PlannedTurns && s.PlannedTurns > 0:
			s.State = "completed"
			e.Outcomes.CompletedSessions++
			if s.StartedAt != nil && s.FinishedAt != nil {
				sessionDurations = append(sessionDurations, DurationMillis(s.FinishedAt.Sub(*s.StartedAt)))
			}
		case s.Failed > 0:
			s.State = "failed"
			s.Skipped = remaining
			e.Outcomes.Skipped += remaining
			e.Outcomes.FailedSessions++
		case canceled || s.Canceled > 0:
			s.State = "canceled"
			e.Outcomes.CanceledBeforeStart += remaining
			e.Outcomes.CanceledSessions++
		default:
			s.State = "incomplete"
		}
	}
	e.Timing.CompletedSessionLatency = summarizeMetric(sessionDurations)
	e.All, e.FirstTurn, e.FollowupTurns = summarizeRequests(requests), summarizeRequests(first), summarizeRequests(follow)
	for turn, rs := range turns {
		e.ByTurn[turn] = summarizeRequests(rs)
	}
	e.Sessions = sessions
	if len(e.Sessions) > MaxEvidenceSessions {
		e.Sessions = e.Sessions[:MaxEvidenceSessions]
		e.SessionsTruncated = true
	}
	return e
}

func summarizeRequests(rs []Request) RequestEvidence {
	e := RequestEvidence{FinishReasons: map[string]int{}, ErrorStages: map[string]int{}}
	e.Tokens.InputSources, e.Tokens.OutputSources = map[string]int{}, map[string]int{}
	var ttft, tpot, latency, completion []float64
	var input, output, context, serverInput, serverOutput, localInput, localOutput, reasoning, failedOutput []int
	cacheRatioOverflow := false
	for _, r := range rs {
		e.Attempted++
		if r.FinishReason != "" {
			e.FinishReasons[r.FinishReason]++
		}
		if r.ErrorStage != "" {
			e.ErrorStages[r.ErrorStage]++
		}
		if r.CachedTokensAvailable {
			e.Cache.Reported++
			if r.Error == nil && !r.Canceled {
				e.Cache.SuccessfulReported++
			}
			if r.ServerInputTokens != nil && !cacheRatioOverflow {
				inputTotal, inputOK := addTokenCount(e.Cache.InputTokens, *r.ServerInputTokens)
				cachedTotal, cachedOK := addTokenCount(e.Cache.CachedTokens, r.CachedTokens)
				if inputOK && cachedOK {
					e.Cache.RatioRequests++
					e.Cache.InputTokens, e.Cache.CachedTokens = inputTotal, cachedTotal
				} else {
					// Zero eligible ratio requests means unavailable, not 0%
					// cache hits. Preserve the independent reporting counts.
					cacheRatioOverflow = true
					e.Cache.RatioRequests, e.Cache.InputTokens, e.Cache.CachedTokens = 0, 0, 0
				}
			}
		} else {
			e.Cache.Unknown++
		}
		if r.Canceled {
			e.Canceled++
		} else if r.Error != nil {
			e.Failed++
		} else {
			e.Success++
		}
		if r.Error != nil || r.Canceled {
			if r.OutputSource != "" {
				failedOutput = append(failedOutput, r.OutputTokens)
			}
			continue
		}
		if !r.FirstOutputAt.IsZero() {
			ttft = append(ttft, DurationMillis(r.TTFT))
		}
		if v, ok := r.TimePerOutputToken(); ok {
			tpot = append(tpot, DurationMillis(v))
		}
		if !r.LastOutputAt.IsZero() {
			latency = append(latency, DurationMillis(r.Latency))
		}
		if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() {
			completion = append(completion, DurationMillis(r.FinishedAt.Sub(r.StartedAt)))
		}
		if r.InputSource != "" {
			input = append(input, r.InputTokens)
			e.Tokens.InputSources[r.InputSource]++
		}
		if r.OutputSource != "" {
			output = append(output, r.OutputTokens)
			e.Tokens.OutputSources[r.OutputSource]++
		}
		if r.InputSource != "" && r.OutputSource != "" {
			context = append(context, r.InputTokens+r.OutputTokens)
		}
		appendOptional := func(v *int, target *[]int) {
			if v != nil {
				*target = append(*target, *v)
			}
		}
		appendOptional(r.ServerInputTokens, &serverInput)
		appendOptional(r.ServerOutputTokens, &serverOutput)
		appendOptional(r.LocalInputTokens, &localInput)
		appendOptional(r.LocalOutputTokens, &localOutput)
		appendOptional(r.ReasoningTokens, &reasoning)
	}
	e.TTFT, e.TPOT, e.LastOutputLatency, e.Completion = summarizeMetric(ttft), summarizeMetric(tpot), summarizeMetric(latency), summarizeMetric(completion)
	e.Tokens.Input, e.Tokens.Output, e.Tokens.Context = summarizeTokens(input), summarizeTokens(output), summarizeTokens(context)
	e.Tokens.ServerInput, e.Tokens.ServerOutput = summarizeTokens(serverInput), summarizeTokens(serverOutput)
	e.Tokens.LocalInput, e.Tokens.LocalOutput = summarizeTokens(localInput), summarizeTokens(localOutput)
	e.Tokens.Reasoning, e.Tokens.FailedOutput = summarizeTokens(reasoning), summarizeTokens(failedOutput)
	return e
}
func summarizeMetric(values []float64) MetricSummary {
	s := MetricSummary{Count: len(values)}
	if len(values) == 0 {
		return s
	}
	sort.Float64s(values)
	for _, v := range values {
		s.MeanMs += v
	}
	s.MeanMs /= float64(len(values))
	s.P50Ms, s.P99Ms = quantile(values, 50), quantile(values, 99)
	return s
}
func summarizeTokens(values []int) TokenDistribution {
	s := TokenDistribution{Count: len(values)}
	if len(values) == 0 {
		return s
	}
	sort.Ints(values)
	s.Min, s.Max = values[0], values[len(values)-1]
	floats := make([]float64, len(values))
	for i, v := range values {
		total, ok := addTokenCount(s.Total, v)
		if !ok {
			// Count=0 is the existing unavailable-distribution contract.
			// A partial or saturated sum would fabricate usable evidence.
			return TokenDistribution{}
		}
		s.Total = total
		floats[i] = float64(v)
	}
	s.P50, s.P99 = quantile(floats, 50), quantile(floats, 99)
	return s
}
func timePointer(t time.Time) *time.Time { t = t.UTC(); return &t }
func requestFinish(r Request) time.Time {
	if !r.FinishedAt.IsZero() {
		return r.FinishedAt
	}
	if r.StartedAt.IsZero() {
		return time.Time{}
	}
	return r.StartedAt.Add(r.Latency)
}

// SessionID names planned sessions independently of worker scheduling.
func SessionID(index int) string { return fmt.Sprintf("session-%06d", index+1) }

func quantile(values []float64, percentile int) float64 {
	if len(values) == 0 {
		return 0
	}
	i := int(math.Ceil(float64(percentile)*float64(len(values))/100)) - 1
	return values[max(0, min(i, len(values)-1))]
}
