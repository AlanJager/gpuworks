package monitor

import (
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file computes Window's engine-histogram-derived fields (TTFT, TPOT,
// E2E, queue/prefill/decode time, preemptions) from a deployment's already-
// scoped WindowSeries. Every histogram family arrives as three quantile-
// labeled gauges per the phase2 metrics contract's "Quantile-at-ingest"
// section (the server never stores raw buckets); filterQuantile splits a
// family's series by its "quantile" label, and the "Window computation from
// quantile gauges" section fixes the reduction: p95/p99 are the max across
// the window (per instance, then across instances - MaxBy then MaxOf, the
// same shape KVUsageMax and MaxTempC already use), p50 is the average.

const (
	quantileP50 = "0.5"
	quantileP95 = "0.95"
	quantileP99 = "0.99"
)

// filterQuantile returns the subset of a histogram family's series carrying
// the given "quantile" label value.
func filterQuantile(series []tsdb.Series, q string) []tsdb.Series {
	return filterByLabel(series, "quantile", q)
}

// reduceP95 combines a family's p95 gauges across every instance/replica
// (MaxBy(..., "") - one series result) and reduces that combined series to
// the window's p95 value.
func reduceP95(series []tsdb.Series) (has bool, value float64) {
	p95 := filterQuantile(series, quantileP95)
	if len(p95) == 0 {
		return false, 0
	}
	combined := MaxBy(p95, "")
	if len(combined) == 0 || len(combined[0].Points) == 0 {
		return false, 0
	}
	return true, MaxOf(Values(combined[0]))
}

// reduceP50 is reduceP95's average-instead-of-max sibling for p50.
func reduceP50(series []tsdb.Series) (has bool, value float64) {
	p50 := filterQuantile(series, quantileP50)
	if len(p50) == 0 {
		return false, 0
	}
	combined := AvgBy(p50, "")
	if len(combined) == 0 || len(combined[0].Points) == 0 {
		return false, 0
	}
	return true, Mean(Values(combined[0]))
}

// reduceP99 mirrors reduceP95 for p99.
func reduceP99(series []tsdb.Series) (has bool, value float64) {
	p99 := filterQuantile(series, quantileP99)
	if len(p99) == 0 {
		return false, 0
	}
	combined := MaxBy(p99, "")
	if len(combined) == 0 || len(combined[0].Points) == 0 {
		return false, 0
	}
	return true, MaxOf(Values(combined[0]))
}

// computeInferHistograms fills TTFT/TPOT/E2E/queue/prefill/decode time and
// preemptions from series. Every family's Has* flag is driven by its own
// p95 presence (the three quantile gauges of one family are always written
// together per tick - see the phase2 metrics contract - so p95's presence
// implies p50/p99's).
func computeInferHistograms(w *Window, series WindowSeries) {
	if has, p95 := reduceP95(series.TTFT); has {
		w.HasTTFT = true
		w.TTFTP95Sec = p95
		_, w.TTFTP50Sec = reduceP50(series.TTFT)
		_, w.TTFTP99Sec = reduceP99(series.TTFT)
	}
	if has, p95 := reduceP95(series.TPOT); has {
		w.HasTPOT = true
		w.TPOTP95Sec = p95
		_, w.TPOTP99Sec = reduceP99(series.TPOT)
	}
	if has, p95 := reduceP95(series.E2E); has {
		w.HasE2E = true
		w.E2EP95Sec = p95
	}
	if has, p95 := reduceP95(series.QueueTime); has {
		w.HasQueueTime = true
		w.QueueTimeP95Sec = p95
	}
	if has, p95 := reduceP95(series.PrefillTime); has {
		w.HasPrefillTime = true
		w.PrefillTimeP95Sec = p95
	}
	if has, p95 := reduceP95(series.DecodeTime); has {
		w.HasDecodeTime = true
		w.DecodeTimeP95Sec = p95
	}
	computePreemptions(w, series.Preemptions)
}

// computePreemptions turns the cumulative gpuworks_infer_preemptions_total
// counter into a per-second rate.
func computePreemptions(w *Window, series []tsdb.Series) {
	if len(series) == 0 {
		return
	}
	rate := Rate(series)
	total := SumBy(rate, "")
	if len(total) == 0 || len(total[0].Points) == 0 {
		return
	}
	w.HasPreemptions = true
	w.PreemptionsPerSec = Mean(Values(total[0]))
}
