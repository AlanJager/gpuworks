package api

import (
	"encoding/json"
	"math"

	"gpuworks.ai/gpuworks/internal/proto"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

// validateBenchmarkLoad checks the advanced load settings of a new run:
// warmup, and open-loop arrivals, which only the parameter-scan mode drives.
// The adaptive modes bisect on concurrency, which an arrival rate replaces.
func validateBenchmarkLoad(mode string, raw json.RawMessage, dc dataset.Config) error {
	doc, err := validateBenchmarkSearch(mode, raw, dc)
	if err != nil {
		return err
	}
	outOfRange := func(field string, min, max float64) error {
		return productError(ErrorBenchmarkLoadValueInvalid, field+" is out of range", map[string]any{"field": field, "min": min, "max": max})
	}
	if doc.WarmupRequests < 0 || doc.WarmupRequests > benchmeta.MaxWarmupRequests {
		return outOfRange("warmup_requests", 0, benchmeta.MaxWarmupRequests)
	}
	if doc.MinSessionCompletion > 0 && mode != "" && mode != string(search.Grid) && mode != string(search.SLA) {
		return benchmarkParameterError("search.min_session_completion", "session completion constraints require parameter-scan or SLA mode")
	}
	if doc.MinSessionCompletion < 0 || doc.MinSessionCompletion > 1 || math.IsNaN(doc.MinSessionCompletion) {
		return outOfRange("min_session_completion", 0, 1)
	}
	if doc.OpenLoop() && mode != "" && mode != string(search.Grid) {
		return productError(ErrorBenchmarkOpenLoopMode, "arrival-rate load is only available in the parameter-scan mode", map[string]any{"mode": mode})
	}
	if len(doc.RequestRates) > benchmeta.MaxRequestRates {
		return outOfRange("request_rates", 1, benchmeta.MaxRequestRates)
	}
	for _, rate := range doc.RequestRates {
		if !(rate > 0) || rate > benchmeta.MaxRequestRate || math.IsInf(rate, 0) {
			return outOfRange("request_rates", 0, benchmeta.MaxRequestRate)
		}
	}
	if doc.Burstiness < 0 || doc.Burstiness > benchmeta.MaxBurstiness {
		return outOfRange("burstiness", 0, benchmeta.MaxBurstiness)
	}
	if doc.PointDurationSeconds != 0 && (doc.PointDurationSeconds < benchmeta.MinPointDurationSeconds || doc.PointDurationSeconds > benchmeta.MaxPointDurationSeconds) {
		return outOfRange("point_duration_s", benchmeta.MinPointDurationSeconds, benchmeta.MaxPointDurationSeconds)
	}
	if doc.MaxInFlight < 0 || doc.MaxInFlight > benchmeta.MaxInFlight {
		return outOfRange("max_in_flight", 0, benchmeta.MaxInFlight)
	}
	return nil
}

// benchmarkLoadSupport checks that the load-origin agent can execute the run.
// An older agent ignores the search fields it does not know. Without
// CapBenchArrival it would run an open-loop grid closed loop, and without
// CapBenchInteractivity it would search past an interactivity floor it never
// checks; both are refused. Without CapBenchWarmup it only skips warmup, which
// costs the first point its cold start, so that is admitted with a run
// warning. The server supports all of them.
func (a *apiHandlers) benchmarkLoadSupport(origin string, raw json.RawMessage) (warnings []string, err error) {
	if origin == "" || origin == "server" || a.d.NodeCaps == nil {
		return nil, nil
	}
	caps := a.d.NodeCaps(origin)
	doc, _ := search.ParseDocument(raw)
	if doc.MinSessionCompletion > 0 && !hasCap(caps, proto.CapBenchEvidence) {
		return nil, productError(ErrorBenchmarkAgentEvidenceUnsupported, "the load-origin agent cannot enforce the session completion constraint; upgrade it first", map[string]any{"node": origin})
	}
	if doc.OpenLoop() && !hasCap(caps, proto.CapBenchArrival) {
		return nil, productError(ErrorBenchmarkAgentArrivalUnsupported, "the load-origin agent predates arrival-rate load; upgrade it first", map[string]any{"node": origin})
	}
	if doc.MinInteractivity > 0 && !hasCap(caps, proto.CapBenchInteractivity) {
		return nil, productError(ErrorBenchmarkAgentInteractivityUnsupported, "the load-origin agent predates the interactivity constraint; upgrade it first", map[string]any{"node": origin})
	}
	if doc.WarmupRequests > 0 && !hasCap(caps, proto.CapBenchWarmup) {
		return []string{proto.BenchmarkWarningWarmupUnsupported}, nil
	}
	return nil, nil
}
