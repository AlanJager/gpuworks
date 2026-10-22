package search

import (
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type Mode string

const (
	Grid       Mode = "grid"
	Saturation Mode = "saturation"
	SLA        Mode = "sla"
)

// Point is one load configuration. A closed-loop point keeps Concurrency
// requests in flight; an open-loop point (RequestRate > 0, req/s) dispatches
// requests by arrival time and uses Concurrency as the in-flight cap, 0 meaning
// unlimited.
type Point struct {
	Concurrency, ISL, OSL int
	RequestRate           float64
}
type SampleCounts struct {
	TTFT, TPOT, Latency int
}

type Observation struct {
	SessionCompletion *float64
	Samples           *SampleCounts
	Point             Point
	// Throughput is output-token throughput. TotalThroughput includes input
	// (prefill) and output tokens over the same measurement window.
	Throughput, TotalThroughput, RequestTput, SuccessRate, TTFT, TPOT, Latency float64
	AvgTTFT, P99TTFT, AvgTPOT, P50TPOT, P99TPOT, AvgLatency, P99Latency        float64
}
type Config struct {
	Mode                      Mode
	Concurrencies, ISLs, OSLs []int
	FixedPoints               bool
	MaxConcurrency            int
	MinThroughputGain         float64
	SLA                       SLAConfig
}
type SLAConfig struct {
	MinSessionCompletion                                                         float64
	MaxAvgTTFT, MaxP99TTFT, MaxAvgTPOT, MaxP99TPOT, MaxAvgLatency, MaxP99Latency float64
	Metric                                                                       string
	MaxRPS, MaxTPS, MinRPS, MinTPS                                               float64
	// MinInteractivity is the lowest acceptable per-user output speed,
	// metrics.Interactivity of the point's P50 TPOT, in tok/s/user.
	MinInteractivity float64
	// The legacy per-user limits judge throughput ÷ concurrency; see Document.
	LegacyMaxPerUserTotalTPS, LegacyMinPerUserTotalTPS   float64
	LegacyMaxPerUserOutputTPS, LegacyMinPerUserOutputTPS float64
	// Objective selects the metric to maximize among feasible SLA points.
	// Supported values are "rps", "output_tps", and "total_tps".
	Objective string
}
type Strategy struct {
	cfg        Config
	queue      []Point
	next       int
	last       *Observation
	tested     map[Point]bool
	stopped    bool
	lower      *Observation
	upper      *Observation
	best       *Observation
	stopReason string
}

func New(c Config) *Strategy {
	// An omitted maximum can grow up to the largest representable concurrency.
	// Store that effective limit so both adaptive modes stop explicitly there.
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = int(^uint(0) >> 1)
	}
	if c.MinThroughputGain <= 0 {
		c.MinThroughputGain = benchmeta.DefaultMinThroughputGain
	}
	s := &Strategy{cfg: c, tested: map[Point]bool{}}
	if c.Mode == Grid || c.Mode == "" || c.FixedPoints {
		for _, cc := range c.Concurrencies {
			for _, i := range c.ISLs {
				for _, o := range c.OSLs {
					s.queue = append(s.queue, Point{Concurrency: cc, ISL: i, OSL: o})
				}
			}
		}
	} else {
		start := 1
		if len(c.Concurrencies) > 0 {
			start = c.Concurrencies[0]
		}
		s.queue = []Point{{Concurrency: start, ISL: first(c.ISLs), OSL: first(c.OSLs)}}
	}
	return s
}

// NewJSON builds the adaptive strategy from the API search payload. Grid
// remains a deterministic matrix; saturation and SLA are generated lazily so
// each next point depends on the previous observation.
func NewJSON(mode string, raw []byte) (*Strategy, error) {
	doc, err := ParseDocument(raw)
	if err != nil {
		return nil, err
	}
	return NewDocument(mode, doc)
}

// NewDocument builds the strategy for a parsed search document.
func NewDocument(mode string, doc Document) (*Strategy, error) {
	conc, err := doc.Concurrencies()
	if err != nil {
		return nil, err
	}
	if len(conc) == 0 {
		conc = []int{1}
	}
	fixedPoints := len(doc.FixedConcurrency) > 0
	if fixedPoints {
		conc = doc.FixedConcurrency
	}
	isl, osl := doc.lengths()
	if mode == "" {
		mode = "grid"
	}
	cfg := Config{Mode: Mode(mode), Concurrencies: conc, ISLs: isl, OSLs: osl, FixedPoints: fixedPoints, MaxConcurrency: doc.MaxConcurrency, MinThroughputGain: doc.MinThroughputGain, SLA: doc.SLA()}
	return New(cfg), nil
}

func first(a []int) int {
	if len(a) == 0 {
		return 1
	}
	return a[0]
}
func (s *Strategy) Next() (Point, bool) {
	if s.stopped && s.next >= len(s.queue) {
		return Point{}, false
	}
	for s.next < len(s.queue) {
		p := s.queue[s.next]
		s.next++
		if !s.tested[p] {
			s.tested[p] = true
			return p, true
		}
	}
	return Point{}, false
}
func (s *Strategy) Planned() int { return len(s.queue) }
func (s *Strategy) Observe(o Observation) {
	if s.stopped {
		return
	}
	prev := s.last
	s.last = &o
	if s.cfg.FixedPoints {
		s.lower = &o
		if s.next >= len(s.queue) {
			s.stopReason = "hit_max_concurrency"
			s.stopped = true
		}
		return
	}
	if s.cfg.Mode == Saturation {
		s.observeSaturation(o, prev)
	} else if s.cfg.Mode == SLA {
		s.observeSLA(o)
	}
}

// Best returns the highest objective value among feasible SLA observations.
// It is nil until at least one point satisfies all constraints.
func (s *Strategy) Best() (Observation, bool) {
	if s.best == nil {
		return Observation{}, false
	}
	return *s.best, true
}

// StopReason explains the condition that ended an adaptive search. It remains
// empty while more points are available.
func (s *Strategy) StopReason() string { return s.stopReason }

// SaturationPoint returns the last successful observation below the upper
// saturation boundary. It is only meaningful for saturation searches.
func (s *Strategy) SaturationPoint() (Observation, bool) {
	if s.cfg.Mode != Saturation || s.lower == nil {
		return Observation{}, false
	}
	return *s.lower, true
}
func (s *Strategy) observeSaturation(o Observation, prev *Observation) {
	if o.SuccessRate < 1 {
		s.stopReason = "error_rate"
		s.upper = &o
		s.enqueueBetween(o)
		return
	}
	if s.upper != nil {
		s.lower = &o
		s.enqueueBetween(o)
		return
	}
	gain := 1.0
	if prev != nil && prev.Throughput > 0 {
		gain = o.Throughput/prev.Throughput - 1
	}
	if gain < s.cfg.MinThroughputGain && prev != nil {
		s.stopReason = "gain_below_threshold"
		s.lower = prev
		s.upper = &o
		s.enqueueBetween(o)
		return
	}
	s.lower = &o
	if s.cfg.MaxConcurrency > 0 && o.Point.Concurrency >= s.cfg.MaxConcurrency {
		s.stopReason = "hit_max_concurrency"
		s.stopped = true
		return
	}
	s.queue = append(s.queue, Point{Concurrency: nextConcurrency(o.Point.Concurrency, s.cfg.MaxConcurrency), ISL: o.Point.ISL, OSL: o.Point.OSL})
}
func (s *Strategy) observeSLA(o Observation) {
	if slaOK(o, s.cfg.SLA) {
		if _, eligible := BestMeasured([]Observation{o}, s.cfg.SLA); eligible && (s.best == nil || betterObservation(o, *s.best, s.cfg.SLA.Objective)) {
			copy := o
			s.best = &copy
		}
		s.lower = &o
		if s.cfg.MaxConcurrency > 0 && o.Point.Concurrency >= s.cfg.MaxConcurrency {
			s.stopReason = "hit_max_concurrency"
			s.stopped = true
			return
		}
		if s.upper != nil {
			s.enqueueBetween(o)
		} else {
			s.queue = append(s.queue, Point{Concurrency: nextConcurrency(o.Point.Concurrency, s.cfg.MaxConcurrency), ISL: o.Point.ISL, OSL: o.Point.OSL})
		}
	} else {
		s.stopReason = "sla_violated"
		s.upper = &o
		s.enqueueBetween(o)
	}
}
func nextConcurrency(c, max int) int {
	if max <= 0 {
		max = int(^uint(0) >> 1)
	}
	if c > max/2 {
		return max
	}
	return c * 2
}
func (s *Strategy) enqueueBetween(o Observation) {
	if s.lower == nil || s.upper == nil {
		s.stopped = true
		return
	}
	if s.upper.Point.Concurrency-s.lower.Point.Concurrency <= 1 {
		s.stopped = true
		return
	}
	mid := midpoint(s.lower.Point.Concurrency, s.upper.Point.Concurrency)
	s.queue = append(s.queue, Point{Concurrency: mid, ISL: o.Point.ISL, OSL: o.Point.OSL})
}
func midpoint(a, b int) int {
	if b <= a || b-a <= 1 {
		return a
	}
	return a + (b-a)/2
}

// Violation is one SLA constraint a point fails. Metric names the measured
// quantity (for example "p99_ttft" or "interactivity"); Actual is unrounded.
type Violation struct {
	Metric string
	Limit  float64
	Actual float64
	Reason string `json:"reason,omitempty"`
}

// Violations lists the constraints of c that observation o fails, in a fixed
// order. It is the only SLA judgement: the search, the persisted verdict and
// the report all use it.
func (c SLAConfig) Violations(o Observation) []Violation {
	avgTTFT, p99TTFT := o.AvgTTFT, o.P99TTFT
	if avgTTFT == 0 {
		avgTTFT = o.TTFT
	}
	if p99TTFT == 0 {
		p99TTFT = o.TTFT
	}
	avgTPOT, p99TPOT := o.AvgTPOT, o.P99TPOT
	if avgTPOT == 0 {
		avgTPOT = o.TPOT
	}
	if p99TPOT == 0 {
		p99TPOT = o.TPOT
	}
	avgLatency, p99Latency := o.AvgLatency, o.P99Latency
	if avgLatency == 0 {
		avgLatency = o.Latency
	}
	if p99Latency == 0 {
		p99Latency = o.Latency
	}
	perUserTotalTPS := perUser(o.TotalThroughput, o.Point.Concurrency)
	perUserOutputTPS := perUser(o.Throughput, o.Point.Concurrency)
	checks := []struct {
		metric        string
		limit, actual float64
		max           bool
	}{
		{"avg_ttft", c.MaxAvgTTFT, avgTTFT, true}, {"p99_ttft", c.MaxP99TTFT, p99TTFT, true},
		{"avg_tpot", c.MaxAvgTPOT, avgTPOT, true}, {"p99_tpot", c.MaxP99TPOT, p99TPOT, true},
		{"avg_latency", c.MaxAvgLatency, avgLatency, true}, {"p99_latency", c.MaxP99Latency, p99Latency, true},
		{"rps", c.MaxRPS, o.RequestTput, true}, {"tps", c.MaxTPS, o.Throughput, true},
		{"rps", c.MinRPS, o.RequestTput, false}, {"tps", c.MinTPS, o.Throughput, false},
		{"interactivity", c.MinInteractivity, metrics.Interactivity(o.P50TPOT), false},
		{"per_user_total_tps", c.LegacyMaxPerUserTotalTPS, perUserTotalTPS, true}, {"per_user_total_tps", c.LegacyMinPerUserTotalTPS, perUserTotalTPS, false},
		{"per_user_output_tps", c.LegacyMaxPerUserOutputTPS, perUserOutputTPS, true}, {"per_user_output_tps", c.LegacyMinPerUserOutputTPS, perUserOutputTPS, false},
	}
	var out []Violation
	for _, check := range checks {
		if check.limit > 0 && !metricAvailable(o, check.metric, check.actual) {
			out = append(out, Violation{Metric: check.metric, Limit: check.limit, Reason: "evidence_unavailable"})
			continue
		}
		if check.limit <= 0 || (check.max && check.actual <= check.limit) || (!check.max && check.actual >= check.limit) {
			continue
		}
		out = append(out, Violation{Metric: check.metric, Limit: check.limit, Actual: check.actual, Reason: "limit_exceeded"})
	}
	if c.MinSessionCompletion > 0 {
		if o.SessionCompletion == nil {
			out = append(out, Violation{Metric: "session_completion", Limit: c.MinSessionCompletion, Reason: "evidence_unavailable"})
		} else if *o.SessionCompletion < c.MinSessionCompletion {
			out = append(out, Violation{Metric: "session_completion", Limit: c.MinSessionCompletion, Actual: *o.SessionCompletion, Reason: "limit_exceeded"})
		}
	}
	return out
}

func slaOK(o Observation, c SLAConfig) bool { return len(c.Violations(o)) == 0 }

func perUser(throughput float64, concurrency int) float64 {
	if concurrency <= 0 {
		return 0
	}
	return throughput / float64(concurrency)
}

func objectiveValue(o Observation, objective string) float64 {
	switch objective {
	case "rps", "request_tps":
		return o.RequestTput
	case "tps", "output_tps", "output_tok_s":
		return o.Throughput
	case "total_tps", "total_tok_s":
		return o.TotalThroughput
	default:
		// Output token throughput is the historical SLA objective and keeps
		// callers that omit objective deterministic.
		return o.Throughput
	}
}
