package search

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
)

// Document is the stored search configuration of a benchmark run: the
// benchmark_runs.search column and the BenchmarkSpec.Search wire field. It is
// read and written whole; every reader decodes it through ParseDocument.
type Document struct {
	// Concurrency is a single starting concurrency (adaptive modes) or a list
	// (grid); both spellings are accepted.
	Concurrency                json.RawMessage `json:"concurrency,omitempty"`
	FixedConcurrency           []int           `json:"fixed_concurrency,omitempty"`
	ISL                        []int           `json:"isl,omitempty"`
	OSL                        []int           `json:"osl,omitempty"`
	MaxConcurrency             int             `json:"max_concurrency,omitempty"`
	MinThroughputGain          float64         `json:"min_throughput_gain,omitempty"`
	RequestsPerPointMultiplier float64         `json:"requests_per_point_multiplier,omitempty"`
	SessionsPerPoint           map[string]int  `json:"sessions_per_point,omitempty"`
	Objective                  string          `json:"objective,omitempty"`
	Metric                     string          `json:"metric,omitempty"`

	// RequestRates switches a grid run to open-loop load: each rate (req/s) is
	// a point dimension in place of concurrency. Requests arrive by a gamma
	// process with shape Burstiness (1 = Poisson), MaxInFlight caps requests in
	// flight (0 = unlimited), and each point lasts PointDurationSeconds of
	// arrivals.
	RequestRates         []float64 `json:"request_rates,omitempty"`
	Burstiness           float64   `json:"burstiness,omitempty"`
	MaxInFlight          int       `json:"max_in_flight,omitempty"`
	PointDurationSeconds float64   `json:"point_duration_s,omitempty"`
	// WarmupRequests run once before the first point; their results are
	// discarded.
	WarmupRequests int `json:"warmup_requests,omitempty"`
	// RequestTimeoutSeconds bounds each request, warmup included; 0 is
	// benchmeta.DefaultRequestTimeoutSeconds (see RequestTimeout).
	RequestTimeoutSeconds int `json:"request_timeout_s,omitempty"`

	MaxAvgTTFT           float64 `json:"max_avg_ttft,omitempty"`
	MaxP99TTFT           float64 `json:"max_p99_ttft,omitempty"`
	MaxAvgTPOT           float64 `json:"max_avg_tpot,omitempty"`
	MaxP99TPOT           float64 `json:"max_p99_tpot,omitempty"`
	MaxAvgLatency        float64 `json:"max_avg_latency,omitempty"`
	MaxP99Latency        float64 `json:"max_p99_latency,omitempty"`
	MaxRPS               float64 `json:"max_rps,omitempty"`
	MaxTPS               float64 `json:"max_tps,omitempty"`
	MinRPS               float64 `json:"min_rps,omitempty"`
	MinTPS               float64 `json:"min_tps,omitempty"`
	MinInteractivity     float64 `json:"min_interactivity,omitempty"`
	MinSessionCompletion float64 `json:"min_session_completion,omitempty"`

	// Legacy per-user constraints divide a point's throughput by its
	// concurrency. New runs no longer offer them; runs that stored them are
	// still judged by that definition so their verdicts do not change.
	LegacyMaxPerUserTotalTPS  float64 `json:"max_per_user_total_tps,omitempty"`
	LegacyMinPerUserTotalTPS  float64 `json:"min_per_user_total_tps,omitempty"`
	LegacyMaxPerUserOutputTPS float64 `json:"max_per_user_output_tps,omitempty"`
	LegacyMinPerUserOutputTPS float64 `json:"min_per_user_output_tps,omitempty"`
}

// ParseDocument decodes a stored search document. An empty input is the
// empty document.
func ParseDocument(raw []byte) (Document, error) {
	var d Document
	if len(raw) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return Document{}, err
	}
	return d, nil
}

// RequestTimeout is the per-request timeout the run applies. A run stored
// without one, created before it was configurable, runs with today's default.
func (d Document) RequestTimeout() time.Duration {
	if d.RequestTimeoutSeconds > 0 {
		return time.Duration(d.RequestTimeoutSeconds) * time.Second
	}
	return benchmeta.DefaultRequestTimeoutSeconds * time.Second
}

// Concurrencies decodes the scalar-or-list concurrency field. A scalar of 0
// means none.
func (d Document) Concurrencies() ([]int, error) {
	raw := d.Concurrency
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var out []int
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	var one int
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	if one > 0 {
		return []int{one}, nil
	}
	return nil, nil
}

// OpenLoop reports whether the document drives load by arrival rate.
func (d Document) OpenLoop() bool { return len(d.RequestRates) > 0 }

// EffectiveBurstiness is the gamma shape of open-loop arrivals.
func (d Document) EffectiveBurstiness() float64 {
	if d.Burstiness <= 0 {
		return benchmeta.DefaultBurstiness
	}
	return d.Burstiness
}

// EffectivePointDuration is the arrival window of one open-loop point.
func (d Document) EffectivePointDuration() float64 {
	if d.PointDurationSeconds <= 0 {
		return benchmeta.DefaultPointDurationSeconds
	}
	return d.PointDurationSeconds
}

// SLA returns the constraint set the document declares.
func (d Document) SLA() SLAConfig {
	return SLAConfig{
		MaxAvgTTFT: d.MaxAvgTTFT, MaxP99TTFT: d.MaxP99TTFT, MaxAvgTPOT: d.MaxAvgTPOT, MaxP99TPOT: d.MaxP99TPOT,
		MaxAvgLatency: d.MaxAvgLatency, MaxP99Latency: d.MaxP99Latency, Metric: d.Metric,
		MaxRPS: d.MaxRPS, MaxTPS: d.MaxTPS, MinRPS: d.MinRPS, MinTPS: d.MinTPS, MinInteractivity: d.MinInteractivity,
		LegacyMaxPerUserTotalTPS: d.LegacyMaxPerUserTotalTPS, LegacyMinPerUserTotalTPS: d.LegacyMinPerUserTotalTPS,
		LegacyMaxPerUserOutputTPS: d.LegacyMaxPerUserOutputTPS, LegacyMinPerUserOutputTPS: d.LegacyMinPerUserOutputTPS,
		Objective: d.Objective, MinSessionCompletion: d.MinSessionCompletion,
	}
}

// RequestsForPoint resolves the number of generated requests (sessions, for a
// multi-turn dataset) for one point. Closed-loop points take an explicit
// per-concurrency count or concurrency × multiplier (default 10 per channel);
// open-loop points take the arrivals of the point duration, at least
// MinOpenLoopRequests.
func (d Document) RequestsForPoint(p Point) (int, error) {
	if p.RequestRate > 0 {
		n, err := requestCount(p.RequestRate * d.EffectivePointDuration())
		if err != nil {
			return 0, err
		}
		if n < benchmeta.MinOpenLoopRequests {
			n = benchmeta.MinOpenLoopRequests
		}
		return n, nil
	}
	if sessions := d.SessionsPerPoint[fmt.Sprint(p.Concurrency)]; sessions > 0 {
		return sessions, nil
	}
	multiplier := d.RequestsPerPointMultiplier
	if multiplier <= 0 {
		multiplier = benchmeta.DefaultRequestsPerPointMultiplier
	}
	concurrency := p.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	return requestCount(float64(concurrency) * multiplier)
}

// float64(MaxInt64) rounds up, so the conversion boundary is exclusive.
func requestCount(value float64) (int, error) {
	value = math.Ceil(value)
	if math.IsNaN(value) || math.IsInf(value, 0) || value >= math.Ldexp(1, strconv.IntSize-1) {
		return 0, fmt.Errorf("generated request count exceeds the supported integer range")
	}
	return maxInt(1, int(value)), nil
}

func (d Document) lengths() ([]int, []int) {
	isl, osl := d.ISL, d.OSL
	if len(isl) == 0 {
		isl = []int{benchmeta.DefaultInputTokens}
	}
	if len(osl) == 0 {
		osl = []int{benchmeta.DefaultOutputTokens}
	}
	return isl, osl
}
