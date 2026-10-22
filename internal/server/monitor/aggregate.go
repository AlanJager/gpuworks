package monitor

import (
	"math"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file holds pure functions over []tsdb.Series / []float64: no I/O, no
// package state. window.go composes them; every function here is safe to
// call with nil/empty input and is table-tested independently in
// aggregate_test.go, including the case tsdb hands back series whose points
// land on different bucket boundaries (a series with no sample in a given
// bucket simply contributes nothing to that bucket, rather than a false
// zero).

// groupSeries buckets series by the value of one label. group == "" (the
// common case for window aggregation, where the caller already narrowed the
// query to one metric/deployment and wants a single combined number per
// bucket) puts every series in the same group under the empty key.
func groupSeries(series []tsdb.Series, label string) map[string][]tsdb.Series {
	groups := map[string][]tsdb.Series{}
	for _, s := range series {
		key := ""
		if label != "" {
			key = s.Labels[label]
		}
		groups[key] = append(groups[key], s)
	}
	return groups
}

// combineByBucket merges every point of every series in group by its exact
// timestamp (the "align by bucket" requirement: two series queried with the
// same step still can't be zipped index-by-index, because either one may be
// missing a bucket the other has data for) and reduces each bucket's
// collected values with reduce.
func combineByBucket(group []tsdb.Series, reduce func([]float64) float64) tsdb.Series {
	byTS := map[int64][]float64{}
	for _, s := range group {
		for _, p := range s.Points {
			byTS[p.TimeMS] = append(byTS[p.TimeMS], p.Value)
		}
	}
	timestamps := make([]int64, 0, len(byTS))
	for ts := range byTS {
		timestamps = append(timestamps, ts)
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
	points := make([]tsdb.Point, 0, len(timestamps))
	for _, ts := range timestamps {
		points = append(points, tsdb.Point{TimeMS: ts, Value: reduce(byTS[ts])})
	}
	return tsdb.Series{Points: points}
}

func groupBy(series []tsdb.Series, label string, reduce func([]float64) float64) []tsdb.Series {
	groups := groupSeries(series, label)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]tsdb.Series, 0, len(keys))
	for _, key := range keys {
		combined := combineByBucket(groups[key], reduce)
		if label != "" {
			combined.Labels = map[string]string{label: key}
		}
		out = append(out, combined)
	}
	return out
}

func sumValues(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func meanValues(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return sumValues(values) / float64(len(values))
}

func maxValues(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	max := values[0]
	for _, v := range values[1:] {
		if v > max {
			max = v
		}
	}
	return max
}

// SumBy groups series by label and sums same-bucket points across each
// group. label == "" sums every input series into one combined series.
func SumBy(series []tsdb.Series, label string) []tsdb.Series {
	return groupBy(series, label, sumValues)
}

// AvgBy groups series by label and averages same-bucket points across each
// group. label == "" averages every input series into one combined series.
func AvgBy(series []tsdb.Series, label string) []tsdb.Series {
	return groupBy(series, label, meanValues)
}

// MaxBy groups series by label and takes the max of same-bucket points
// across each group. label == "" takes the max across every input series.
func MaxBy(series []tsdb.Series, label string) []tsdb.Series {
	return groupBy(series, label, maxValues)
}

// resetAwareStepDelta returns the delta between two consecutive counter
// readings, treating a decrease as a counter reset (restart, or process
// replaced): the counter restarted at or near zero, so the value actually
// accumulated since the reset is just the new reading, not a negative
// contribution. Rate and window.go's SumSeriesDelta share this so a counter
// reset is never invisible to one but visible to the other.
func resetAwareStepDelta(prev, cur float64) float64 {
	delta := cur - prev
	if delta < 0 {
		delta = cur
	}
	return delta
}

// Rate computes a per-second counter rate for each input series
// independently (it does not combine series - callers that want a combined
// rate call SumBy on Rate's output), applying resetAwareStepDelta between
// each pair of consecutive points. The output has one point fewer than the
// input per series (a rate needs two samples); zero- or one-point series
// produce no output points.
func Rate(series []tsdb.Series) []tsdb.Series {
	out := make([]tsdb.Series, 0, len(series))
	for _, s := range series {
		points := make([]tsdb.Point, 0, len(s.Points))
		for i := 1; i < len(s.Points); i++ {
			prev, cur := s.Points[i-1], s.Points[i]
			dtSeconds := float64(cur.TimeMS-prev.TimeMS) / 1000
			if dtSeconds <= 0 {
				continue
			}
			delta := resetAwareStepDelta(prev.Value, cur.Value)
			points = append(points, tsdb.Point{TimeMS: cur.TimeMS, Value: delta / dtSeconds})
		}
		out = append(out, tsdb.Series{Labels: s.Labels, Points: points})
	}
	return out
}

// Quantile returns the q-quantile (0..1) of values using linear
// interpolation between closest ranks. An empty slice returns 0; q is
// clamped to [0,1].
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// Values extracts one series' point values in time order.
func Values(s tsdb.Series) []float64 {
	out := make([]float64, len(s.Points))
	for i, p := range s.Points {
		out[i] = p.Value
	}
	return out
}

// Mean is the arithmetic mean of values, or 0 for an empty slice.
func Mean(values []float64) float64 { return meanValues(values) }

// MaxOf is the maximum of values, or 0 for an empty slice.
func MaxOf(values []float64) float64 { return maxValues(values) }

// LatestValue returns the most recent point across every series (by
// TimeMS), independent of which series it came from. It answers "what is
// the freshest data we have", used for snapshot AsOf/Stale computation.
func LatestValue(series []tsdb.Series) (tsdb.Point, bool) {
	var latest tsdb.Point
	found := false
	for _, s := range series {
		for _, p := range s.Points {
			if !found || p.TimeMS > latest.TimeMS {
				latest = p
				found = true
			}
		}
	}
	return latest, found
}
