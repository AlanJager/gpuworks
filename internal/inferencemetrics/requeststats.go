package inferencemetrics

import (
	"regexp"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
)

// Request stats are what an engine reports about one request inside its own
// reply (the last event of a stream, or the body of a plain reply), as
// opposed to its Prometheus exposition: vLLM's per-request timing metrics,
// TensorFold's "tensorfold" object, llama.cpp's "timings". The gateway reads
// them as each reply ends, so every request carries its own queue, prefill and
// decode time without the platform asking the engine anything.

// RequestStat is one per-request figure a reply may carry.
type RequestStat struct {
	// Name is the short name a template's api.request_stats maps a reply
	// field to.
	Name string `json:"name"`
	// Unit is seconds for the phases, tokens for counts.
	Unit string `json:"unit"`
}

const (
	// RequestQueueSeconds is the time the request waited before the engine
	// scheduled it.
	RequestQueueSeconds = "queue_seconds"
	// RequestPrefillSeconds is the time from scheduling to the first token.
	RequestPrefillSeconds = "prefill_seconds"
	// RequestDecodeSeconds is the time from the first token to the last.
	RequestDecodeSeconds = "decode_seconds"
)

var requestStats = []RequestStat{
	{RequestQueueSeconds, "seconds"},
	{RequestPrefillSeconds, "seconds"},
	{RequestDecodeSeconds, "seconds"},
}

// RequestStats returns the request-stat vocabulary sorted by name.
func RequestStats() []RequestStat {
	out := append([]RequestStat(nil), requestStats...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsRequestStat reports whether name is a request stat.
func IsRequestStat(name string) bool {
	for _, stat := range requestStats {
		if stat.Name == name {
			return true
		}
	}
	return false
}

// RequestStatRule reads one reply field into a request stat. Path is the
// field's dotted key path from the reply event's top-level object
// (metrics.queue_time_ms); Scale multiplies the value (0 means 1), so an
// engine reporting milliseconds maps onto seconds with 0.001.
type RequestStatRule struct {
	Path  string
	Stat  string
	Scale float64
}

// Factor is the rule's effective multiplier.
func (r RequestStatRule) Factor() float64 {
	if r.Scale == 0 {
		return 1
	}
	return r.Scale
}

// Keys is the path split into its keys.
func (r RequestStatRule) Keys() []string { return strings.Split(r.Path, ".") }

var requestStatPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// ValidateRequestStatRules checks rules against the vocabulary: every path a
// dotted key path, every target a request stat, no negative scale and no
// path stated twice.
func ValidateRequestStatRules(rules []RequestStatRule) error {
	seen := map[string]bool{}
	for _, rule := range rules {
		switch {
		case !requestStatPath.MatchString(rule.Path):
			return &RuleError{Problem: ProblemBadJSONPath, Source: rule.Path, Metric: rule.Stat}
		case !IsRequestStat(rule.Stat):
			return &RuleError{Problem: ProblemUnknownMetric, Source: rule.Path, Metric: rule.Stat}
		case rule.Scale < 0:
			return &RuleError{Problem: ProblemNegativeScale, Source: rule.Path, Metric: rule.Stat}
		case seen[rule.Path]:
			return &RuleError{Problem: ProblemDuplicateSource, Source: rule.Path, Metric: rule.Stat}
		}
		seen[rule.Path] = true
	}
	return nil
}

// engineRequestStats are the request stats of engines the platform knows.
// vLLM returns them, in milliseconds, when it runs with
// --enable-per-request-metrics (v0.25.0 on), on the reply that carries usage.
var engineRequestStats = map[string][]RequestStatRule{
	engines.VLLM: {
		{Path: "metrics.queue_time_ms", Stat: RequestQueueSeconds, Scale: 0.001},
		{Path: "metrics.time_to_first_token_ms", Stat: RequestPrefillSeconds, Scale: 0.001},
		{Path: "metrics.generation_time_ms", Stat: RequestDecodeSeconds, Scale: 0.001},
	},
}

// EngineRequestStats returns an engine's built-in request-stat rules; nil for
// an engine whose replies carry none the platform knows.
func EngineRequestStats(engine string) []RequestStatRule {
	return append([]RequestStatRule(nil), engineRequestStats[strings.ToLower(strings.TrimSpace(engine))]...)
}
