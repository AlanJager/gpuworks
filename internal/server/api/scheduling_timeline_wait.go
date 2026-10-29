package api

import (
	"sort"
	"strconv"
	"time"
)

// The gateway counts the requests it holds for a sleeping deployment and how
// long each waits (wake_hold_wait_ms, flushed as a count and as the quantiles
// of each flush interval). A window's figures are read back from them.
const (
	holdWaitMetric = "gpuworks_proxy_wake_hold_wait_ms"
	// holdWaitStep is the resolution the series are read at.
	holdWaitStep = 5 * time.Minute
)

// holdWait is a deployment's held requests over a window. The quantiles are
// nil when nothing was held or the series are not kept.
type holdWait struct {
	held     int
	p50, p95 *float64
}

// holdWaits reads each deployment's held requests and waits over [from, to].
// The count is exact (the rise of the cumulative count, a restart of the
// server counting from zero again). The quantiles are those of the flush
// intervals weighted by the requests of each: the stored series keep no
// buckets, so a window's own quantile cannot be recomputed and this is an
// approximation, which the API documents.
func (a *apiHandlers) holdWaits(ids []uint, from, to time.Time) map[uint]holdWait {
	out := map[uint]holdWait{}
	if a.d.MetricsQuery == nil {
		return out
	}
	step := holdWaitStep.Milliseconds()
	start, end := from.Add(-holdWaitStep).UnixMilli(), to.UnixMilli()
	for _, id := range ids {
		dep := strconv.FormatUint(uint64(id), 10)
		counts, err := a.d.MetricsQuery(holdWaitMetric+"_count", map[string]string{"deployment": dep}, start, end, step)
		if err != nil || len(counts) == 0 {
			continue
		}
		weights := map[int64]float64{} // bucket time -> requests held in it
		var held float64
		for _, series := range counts {
			var prev float64
			for _, point := range series.Points {
				ts, value := int64(point[0]), point[1]
				rise := value - prev
				if rise < 0 {
					rise = value // the counter restarted
				}
				prev = value
				if ts < from.UnixMilli() {
					continue // the baseline before the window
				}
				weights[ts] += rise
				held += rise
			}
		}
		result := holdWait{held: int(held + 0.5)}
		for quantile, into := range map[string]**float64{"0.5": &result.p50, "0.95": &result.p95} {
			series, err := a.d.MetricsQuery(holdWaitMetric, map[string]string{"deployment": dep, "quantile": quantile}, from.UnixMilli(), end, step)
			if err != nil {
				continue
			}
			var points []weighted
			for _, s := range series {
				for _, point := range s.Points {
					if w := weights[int64(point[0])]; w > 0 {
						points = append(points, weighted{value: point[1], weight: w})
					}
				}
			}
			if v, ok := weightedQuantile(points, quantileOf(quantile)); ok {
				*into = &v
			}
		}
		out[id] = result
	}
	return out
}

func quantileOf(label string) float64 {
	q, _ := strconv.ParseFloat(label, 64)
	return q
}

type weighted struct{ value, weight float64 }

// weightedQuantile is the smallest value at which the weights up to it reach
// q of the total.
func weightedQuantile(points []weighted, q float64) (float64, bool) {
	var total float64
	for _, p := range points {
		total += p.weight
	}
	if total <= 0 {
		return 0, false
	}
	sort.Slice(points, func(i, j int) bool { return points[i].value < points[j].value })
	var run float64
	for _, p := range points {
		run += p.weight
		if run >= q*total {
			return p.value, true
		}
	}
	return points[len(points)-1].value, true
}
