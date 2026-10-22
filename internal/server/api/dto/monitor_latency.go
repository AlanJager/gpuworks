package dto

// MonitorLatency is GET /monitor/deployments/{id}/latency: where the time of
// the deployment's whole replies (completed or stopped at the length limit)
// went within the range. The segments' means add up to the end-to-end mean,
// so a bar drawn from them is the end-to-end time split.
type MonitorLatency struct {
	FromMS int64 `json:"from_ms"`
	ToMS   int64 `json:"to_ms"`
	// Requests counts the whole replies in the range.
	Requests int64 `json:"requests"`
	// EndToEnd is the gateway's end-to-end time; null without a request.
	EndToEnd *MonitorLatencyStat `json:"end_to_end"`
	// Segments are the phases the replies went through, in order, each
	// with a share of the end-to-end mean above zero.
	Segments []MonitorLatencySegment `json:"segments"`
	// Speeds are tokens per second, each the ratio of the tokens and the
	// time of the replies that gave both; null when none did.
	Speeds MonitorLatencySpeeds `json:"speeds"`
}

// MonitorLatencyStat is a time's mean and percentiles in milliseconds. The
// percentiles come from quarter-octave bins, within about 10%.
type MonitorLatencyStat struct {
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
}

// MonitorLatencySegment is one phase: gateway, queue, prefill, waiting,
// thinking, answering or tail.
type MonitorLatencySegment struct {
	Kind string `json:"kind"`
	// MeanMS is the phase's mean over every whole reply (0 for a reply
	// without it), so the means add up to the end-to-end mean.
	MeanMS float64 `json:"mean_ms"`
	// Requests counts the replies that had the phase; P50MS and P95MS are
	// over those.
	Requests int64   `json:"requests"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
}

// MonitorLatencySpeeds are the mean speeds in tokens per second: prefill
// over the uncached prompt, thinking over the reasoning tokens, answering
// over the rest of the output, and the whole decode.
type MonitorLatencySpeeds struct {
	PrefillTokS   *float64 `json:"prefill_tok_s"`
	ThinkingTokS  *float64 `json:"thinking_tok_s"`
	AnsweringTokS *float64 `json:"answering_tok_s"`
	DecodeTokS    *float64 `json:"decode_tok_s"`
}
