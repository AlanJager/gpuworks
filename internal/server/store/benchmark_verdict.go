package store

import (
	"encoding/json"
	"math"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

// BenchmarkVerdict is the server's durable, objective interpretation of a
// terminal benchmark. Suggestions intentionally remain a UI concern.
type BenchmarkVerdict struct {
	Kind                  string                      `json:"kind"`
	BestPointSeq          int                         `json:"best_point_seq,omitempty"`
	SaturationConcurrency int                         `json:"saturation_concurrency,omitempty"`
	HeadroomPct           float64                     `json:"headroom_pct,omitempty"`
	SLAMaxConcurrency     int                         `json:"sla_max_concurrency,omitempty"`
	Violated              []BenchmarkVerdictViolation `json:"violated,omitempty"`
}

type BenchmarkVerdictViolation struct {
	Metric string  `json:"metric"`
	Limit  float64 `json:"limit"`
	Actual float64 `json:"actual"`
	Reason string  `json:"reason,omitempty"`
}

func computeBenchmarkVerdict(run *BenchmarkRun, points []BenchmarkPoint, st proto.BenchmarkStatus) (BenchmarkVerdict, bool) {
	if st.State == "failed" {
		return BenchmarkVerdict{Kind: "failed", BestPointSeq: bestOutputPoint(points).Seq}, true
	}
	if st.State != "done" || st.StopReason == "" {
		return BenchmarkVerdict{}, false
	}
	if run.Mode == "" || run.Mode == string(search.Grid) {
		if st.StopReason != proto.BenchmarkStopGridExhausted {
			return BenchmarkVerdict{}, false
		}
		doc, err := search.ParseDocument([]byte(run.Search))
		if err != nil {
			return BenchmarkVerdict{}, false
		}
		observations := make([]search.Observation, 0, len(points))
		seqByObservation := make(map[search.Observation]int, len(points))
		for _, point := range points {
			if point.State == "canceled" || point.State == "failed" {
				continue
			}
			observation := benchmarkObservation(point)
			observations = append(observations, observation)
			if _, exists := seqByObservation[observation]; !exists {
				seqByObservation[observation] = point.Seq
			}
		}
		verdict := BenchmarkVerdict{Kind: "grid_done"}
		if best, ok := search.BestMeasured(observations, doc.SLA()); ok {
			verdict.BestPointSeq = seqByObservation[best]
		}
		return verdict, true
	}

	strategy, err := search.NewJSON(run.Mode, []byte(run.Search))
	if err != nil {
		return BenchmarkVerdict{}, false
	}
	seqByObservation := make(map[search.Observation]int, len(points))
	for _, point := range points {
		observation := benchmarkObservation(point)
		if _, exists := seqByObservation[observation]; !exists {
			seqByObservation[observation] = point.Seq
		}
		strategy.Observe(observation)
	}

	if run.Mode == string(search.Saturation) {
		lastGain := throughputGainPct(points)
		if st.StopReason == proto.BenchmarkStopHitMaxConcurrency && lastGain >= minThroughputGainPct(run.Search) {
			last := highestConcurrencyPoint(points)
			return BenchmarkVerdict{Kind: "truncated", BestPointSeq: last.Seq, SaturationConcurrency: last.Concurrency, HeadroomPct: lastGain}, true
		}
		best, ok := strategy.SaturationPoint()
		if !ok {
			p := bestOutputPoint(points)
			return BenchmarkVerdict{Kind: "saturated", BestPointSeq: p.Seq, SaturationConcurrency: p.Concurrency, HeadroomPct: lastGain}, true
		}
		return BenchmarkVerdict{Kind: "saturated", BestPointSeq: seqByObservation[best], SaturationConcurrency: best.Point.Concurrency, HeadroomPct: lastGain}, true
	}

	if run.Mode == string(search.SLA) {
		if best, ok := strategy.Best(); ok {
			return BenchmarkVerdict{Kind: "sla_met", BestPointSeq: seqByObservation[best], SLAMaxConcurrency: best.Point.Concurrency}, true
		}
		closest := closestSLAPoint(run.Search, points)
		return BenchmarkVerdict{Kind: "sla_none", BestPointSeq: closest.Seq, Violated: benchmarkSLAViolations(run.Search, closest)}, true
	}
	return BenchmarkVerdict{}, false
}

func benchmarkObservation(p BenchmarkPoint) search.Observation {
	total := p.Success + p.Errors
	successRate := float64(0)
	if total > 0 {
		successRate = float64(p.Success) / float64(total)
	}
	return search.WithEvidence(search.Observation{
		Point:      benchmarkSearchPoint(p),
		Throughput: p.OutputTput, TotalThroughput: p.TotalTput, RequestTput: p.RequestTput,
		SuccessRate: successRate, AvgTTFT: p.AvgTTFT, P99TTFT: p.P99TTFT,
		AvgTPOT: p.AvgTPOT, P50TPOT: p.P50TPOT, P99TPOT: p.P99TPOT, AvgLatency: p.AvgLatency, P99Latency: p.P99Latency,
	}, json.RawMessage(p.LongTail))
}

func benchmarkSearchPoint(p BenchmarkPoint) search.Point {
	return search.Point{Concurrency: p.Concurrency, ISL: p.TargetISL, OSL: p.TargetOSL, RequestRate: p.RequestRate}
}

func bestOutputPoint(points []BenchmarkPoint) BenchmarkPoint {
	var best BenchmarkPoint
	for _, p := range points {
		if p.State != "" && p.State != "done" {
			continue
		}
		if best.Seq == 0 || p.OutputTput > best.OutputTput {
			best = p
		}
	}
	return best
}

func highestConcurrencyPoint(points []BenchmarkPoint) BenchmarkPoint {
	var best BenchmarkPoint
	for _, p := range points {
		if best.Seq == 0 || p.Concurrency > best.Concurrency {
			best = p
		}
	}
	return best
}

func throughputGainPct(points []BenchmarkPoint) float64 {
	ordered := append([]BenchmarkPoint(nil), points...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Concurrency < ordered[j].Concurrency })
	if len(ordered) < 2 || ordered[len(ordered)-2].OutputTput <= 0 {
		return 0
	}
	return round2((ordered[len(ordered)-1].OutputTput/ordered[len(ordered)-2].OutputTput - 1) * 100)
}

func minThroughputGainPct(raw string) float64 {
	var cfg struct {
		Value float64 `json:"min_throughput_gain"`
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	if cfg.Value <= 0 {
		cfg.Value = .05
	}
	return cfg.Value * 100
}

func benchmarkSLAViolations(raw string, p BenchmarkPoint) []BenchmarkVerdictViolation {
	doc, _ := search.ParseDocument([]byte(raw))
	var out []BenchmarkVerdictViolation
	for _, v := range doc.SLA().Violations(benchmarkObservation(p)) {
		out = append(out, BenchmarkVerdictViolation{Metric: v.Metric, Limit: v.Limit, Actual: round2(v.Actual), Reason: v.Reason})
	}
	return out
}

func closestSLAPoint(raw string, points []BenchmarkPoint) BenchmarkPoint {
	var best BenchmarkPoint
	bestDistance := math.Inf(1)
	for _, p := range points {
		distance := float64(len(benchmarkSLAViolations(raw, p)))
		if distance < bestDistance || (distance == bestDistance && p.Concurrency > best.Concurrency) {
			best, bestDistance = p, distance
		}
	}
	return best
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
