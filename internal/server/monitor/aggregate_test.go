package monitor

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func pt(ts int64, v float64) tsdb.Point { return tsdb.Point{TimeMS: ts, Value: v} }

func TestSumByEmptyInput(t *testing.T) {
	if out := SumBy(nil, ""); len(out) != 0 {
		t.Fatalf("SumBy(nil) = %v, want empty", out)
	}
}

func TestSumByCombinesAllWhenLabelEmpty(t *testing.T) {
	series := []tsdb.Series{
		{Labels: map[string]string{"instance": "a"}, Points: []tsdb.Point{pt(1000, 2), pt(2000, 3)}},
		{Labels: map[string]string{"instance": "b"}, Points: []tsdb.Point{pt(1000, 5), pt(2000, 7)}},
	}
	out := SumBy(series, "")
	if len(out) != 1 {
		t.Fatalf("SumBy(\"\") groups = %d, want 1", len(out))
	}
	if got := Values(out[0]); len(got) != 2 || got[0] != 7 || got[1] != 10 {
		t.Fatalf("SumBy(\"\") values = %v, want [7 10]", got)
	}
}

func TestSumByAlignsMisalignedBuckets(t *testing.T) {
	// Series b is missing the t=2000 bucket and has an extra t=3000 bucket
	// neither of the others has - the combined series must not silently
	// treat the missing side as zero, and must not drop the unique bucket.
	series := []tsdb.Series{
		{Points: []tsdb.Point{pt(1000, 1), pt(2000, 1)}},
		{Points: []tsdb.Point{pt(1000, 10), pt(3000, 10)}},
	}
	out := SumBy(series, "")
	if len(out) != 1 {
		t.Fatalf("groups = %d, want 1", len(out))
	}
	want := map[int64]float64{1000: 11, 2000: 1, 3000: 10}
	for _, p := range out[0].Points {
		if want[p.TimeMS] != p.Value {
			t.Errorf("bucket %d = %v, want %v", p.TimeMS, p.Value, want[p.TimeMS])
		}
	}
	if len(out[0].Points) != 3 {
		t.Fatalf("points = %d, want 3", len(out[0].Points))
	}
}

func TestSumByGroupsByLabel(t *testing.T) {
	series := []tsdb.Series{
		{Labels: map[string]string{"node": "n1"}, Points: []tsdb.Point{pt(1000, 2)}},
		{Labels: map[string]string{"node": "n1"}, Points: []tsdb.Point{pt(1000, 3)}},
		{Labels: map[string]string{"node": "n2"}, Points: []tsdb.Point{pt(1000, 100)}},
	}
	out := SumBy(series, "node")
	if len(out) != 2 {
		t.Fatalf("groups = %d, want 2", len(out))
	}
	byNode := map[string]float64{}
	for _, s := range out {
		byNode[s.Labels["node"]] = s.Points[0].Value
	}
	if byNode["n1"] != 5 || byNode["n2"] != 100 {
		t.Fatalf("byNode = %v", byNode)
	}
}

func TestAvgByAverages(t *testing.T) {
	series := []tsdb.Series{
		{Points: []tsdb.Point{pt(1000, 2)}},
		{Points: []tsdb.Point{pt(1000, 8)}},
	}
	out := AvgBy(series, "")
	if len(out) != 1 || out[0].Points[0].Value != 5 {
		t.Fatalf("AvgBy = %v, want 5", out)
	}
}

func TestMaxByTakesMax(t *testing.T) {
	series := []tsdb.Series{
		{Points: []tsdb.Point{pt(1000, 2), pt(2000, 90)}},
		{Points: []tsdb.Point{pt(1000, 8), pt(2000, 10)}},
	}
	out := MaxBy(series, "")
	if len(out) != 1 {
		t.Fatalf("groups = %d", len(out))
	}
	if out[0].Points[0].Value != 8 || out[0].Points[1].Value != 90 {
		t.Fatalf("MaxBy points = %v", out[0].Points)
	}
}

func TestRateEmptyAndSinglePoint(t *testing.T) {
	if out := Rate(nil); len(out) != 0 {
		t.Fatalf("Rate(nil) = %v", out)
	}
	out := Rate([]tsdb.Series{{Points: []tsdb.Point{pt(1000, 5)}}})
	if len(out) != 1 || len(out[0].Points) != 0 {
		t.Fatalf("Rate(single point) = %v, want no output points", out)
	}
}

func TestRateSteadyIncrease(t *testing.T) {
	series := []tsdb.Series{{Points: []tsdb.Point{pt(0, 0), pt(10_000, 100), pt(20_000, 300)}}}
	out := Rate(series)
	if len(out[0].Points) != 2 {
		t.Fatalf("points = %d, want 2", len(out[0].Points))
	}
	if got := out[0].Points[0].Value; got != 10 {
		t.Errorf("rate[0] = %v, want 10 (100/10s)", got)
	}
	if got := out[0].Points[1].Value; got != 20 {
		t.Errorf("rate[1] = %v, want 20 (200/10s)", got)
	}
}

func TestRateHandlesCounterReset(t *testing.T) {
	// Value drops from 100 to 5: treated as a reset, rate computed from zero
	// using the new value alone, never negative.
	series := []tsdb.Series{{Points: []tsdb.Point{pt(0, 100), pt(10_000, 5)}}}
	out := Rate(series)
	if len(out[0].Points) != 1 {
		t.Fatalf("points = %d, want 1", len(out[0].Points))
	}
	if got := out[0].Points[0].Value; got != 0.5 {
		t.Errorf("rate after reset = %v, want 0.5 (5/10s)", got)
	}
}

func TestRateSkipsNonPositiveDelta(t *testing.T) {
	series := []tsdb.Series{{Points: []tsdb.Point{pt(1000, 1), pt(1000, 2)}}}
	out := Rate(series)
	if len(out[0].Points) != 0 {
		t.Fatalf("points = %v, want none for a zero-duration gap", out[0].Points)
	}
}

func TestResetAwareStepDelta(t *testing.T) {
	if got := resetAwareStepDelta(10, 40); got != 30 {
		t.Errorf("increase: got %v, want 30", got)
	}
	if got := resetAwareStepDelta(40, 10); got != 10 {
		t.Errorf("reset (decrease): got %v, want 10 (the new reading alone)", got)
	}
	if got := resetAwareStepDelta(10, 10); got != 0 {
		t.Errorf("unchanged: got %v, want 0", got)
	}
}

// TestRateHandlesMidSeriesReset is F3's regression case: a reset that
// happens and partially recovers in the middle of a longer series must be
// visible at every step, not just when comparing the series' first and last
// points.
func TestRateHandlesMidSeriesReset(t *testing.T) {
	series := []tsdb.Series{{Points: []tsdb.Point{
		pt(0, 1000), pt(10_000, 20), pt(20_000, 60), pt(30_000, 1100),
	}}}
	out := Rate(series)
	if len(out[0].Points) != 3 {
		t.Fatalf("points = %d, want 3", len(out[0].Points))
	}
	want := []float64{2, 4, 104} // 20/10s, 40/10s, 1040/10s
	for i, w := range want {
		if got := out[0].Points[i].Value; got != w {
			t.Errorf("rate[%d] = %v, want %v", i, got, w)
		}
	}
}

func TestQuantileEmpty(t *testing.T) {
	if got := Quantile(nil, 0.95); got != 0 {
		t.Fatalf("Quantile(nil) = %v, want 0", got)
	}
}

func TestQuantileBoundsAndInterpolation(t *testing.T) {
	values := []float64{10, 20, 30, 40}
	if got := Quantile(values, 0); got != 10 {
		t.Errorf("q0 = %v, want 10", got)
	}
	if got := Quantile(values, 1); got != 40 {
		t.Errorf("q1 = %v, want 40", got)
	}
	// Linear interpolation: position = 0.5*3 = 1.5, between index 1 (20) and
	// index 2 (30).
	if got := Quantile(values, 0.5); got != 25 {
		t.Errorf("q0.5 = %v, want 25", got)
	}
}

func TestQuantileClampsOutOfRangeQ(t *testing.T) {
	values := []float64{1, 2, 3}
	if got := Quantile(values, -1); got != 1 {
		t.Errorf("q<0 = %v, want clamped to min", got)
	}
	if got := Quantile(values, 2); got != 3 {
		t.Errorf("q>1 = %v, want clamped to max", got)
	}
}

func TestValuesMeanMaxOf(t *testing.T) {
	if got := Mean(nil); got != 0 {
		t.Errorf("Mean(nil) = %v, want 0", got)
	}
	if got := MaxOf(nil); got != 0 {
		t.Errorf("MaxOf(nil) = %v, want 0", got)
	}
	s := tsdb.Series{Points: []tsdb.Point{pt(0, 1), pt(1, 5), pt(2, 3)}}
	values := Values(s)
	if len(values) != 3 {
		t.Fatalf("Values = %v", values)
	}
	if got := Mean(values); got != 3 {
		t.Errorf("Mean = %v, want 3", got)
	}
	if got := MaxOf(values); got != 5 {
		t.Errorf("MaxOf = %v, want 5", got)
	}
}

func TestLatestValueEmpty(t *testing.T) {
	if _, ok := LatestValue(nil); ok {
		t.Fatal("LatestValue(nil) should report not-found")
	}
}

func TestLatestValueAcrossSeries(t *testing.T) {
	series := []tsdb.Series{
		{Points: []tsdb.Point{pt(1000, 1), pt(5000, 2)}},
		{Points: []tsdb.Point{pt(3000, 9)}},
	}
	p, ok := LatestValue(series)
	if !ok || p.TimeMS != 5000 || p.Value != 2 {
		t.Fatalf("LatestValue = %+v, ok=%v, want ts=5000 v=2", p, ok)
	}
}
