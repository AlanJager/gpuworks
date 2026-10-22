package search

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// WithEvidence enriches an observation from the optional versioned long-tail
// facts. Unknown versions and historical documents retain their old values.
func WithEvidence(o Observation, longTail json.RawMessage) Observation {
	var document struct {
		Evidence *metrics.PointEvidence `json:"Evidence"`
	}
	if json.Unmarshal(longTail, &document) != nil || document.Evidence == nil || document.Evidence.Version != 1 {
		return o
	}
	e := document.Evidence
	o.TTFT, o.AvgTTFT, o.P99TTFT = e.All.TTFT.P50Ms, e.All.TTFT.MeanMs, e.All.TTFT.P99Ms
	o.TPOT, o.AvgTPOT, o.P50TPOT, o.P99TPOT = e.All.TPOT.P50Ms, e.All.TPOT.MeanMs, e.All.TPOT.P50Ms, e.All.TPOT.P99Ms
	o.Latency, o.AvgLatency, o.P99Latency = e.All.LastOutputLatency.P50Ms, e.All.LastOutputLatency.MeanMs, e.All.LastOutputLatency.P99Ms
	o.Samples = &SampleCounts{TTFT: e.All.TTFT.Count, TPOT: e.All.TPOT.Count, Latency: e.All.LastOutputLatency.Count}
	if e.Plan.PlannedSessions > 0 {
		ratio := float64(e.Outcomes.CompletedSessions) / float64(e.Plan.PlannedSessions)
		o.SessionCompletion = &ratio
	}
	return o
}
