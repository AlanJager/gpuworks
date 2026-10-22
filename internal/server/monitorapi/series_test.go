package monitorapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func TestGroupSeriesByAggregations(t *testing.T) {
	series := []tsdb.Series{
		{Labels: map[string]string{"node": "a"}, Points: []tsdb.Point{{TimeMS: 0, Value: 10}, {TimeMS: 1000, Value: 20}}},
		{Labels: map[string]string{"node": "b"}, Points: []tsdb.Point{{TimeMS: 0, Value: 1}, {TimeMS: 1000, Value: 2}}},
	}
	if sum := groupSeriesBy(series, "", "sum"); len(sum) != 1 || sum[0].Points[0].Value != 11 {
		t.Fatalf("sum by \"\" = %+v", sum)
	}
	if max := groupSeriesBy(series, "", "max"); len(max) != 1 || max[0].Points[0].Value != 10 {
		t.Fatalf("max by \"\" = %+v", max)
	}
	if avg := groupSeriesBy(series, "", "avg"); len(avg) != 1 || avg[0].Points[0].Value != 5.5 {
		t.Fatalf("avg (default) by \"\" = %+v", avg)
	}
	if byNode := groupSeriesBy(series, "node", "avg"); len(byNode) != 2 {
		t.Fatalf("avg by node = %+v, want one series per node", byNode)
	}
	counter := []tsdb.Series{{Points: []tsdb.Point{{TimeMS: 0, Value: 0}, {TimeMS: 1000, Value: 10}}}}
	if rate := groupSeriesBy(counter, "", "rate"); len(rate) != 1 || len(rate[0].Points) != 1 || rate[0].Points[0].Value != 10 {
		t.Fatalf("rate = %+v, want 10/s over the 1s gap", rate)
	}
}

// TestGroupSeriesByGPUPrefersUUIDOverBareIndex proves two different nodes'
// index-0 card must never be merged into one series just because they
// share the same bare "gpu" index label.
func TestGroupSeriesByGPUPrefersUUIDOverBareIndex(t *testing.T) {
	series := []tsdb.Series{
		{Labels: map[string]string{"node": "node-a", "gpu": "0", "gpu_uuid": "GPU-AAA"}, Points: []tsdb.Point{{TimeMS: 0, Value: 91}}},
		{Labels: map[string]string{"node": "node-b", "gpu": "0", "gpu_uuid": "GPU-BBB"}, Points: []tsdb.Point{{TimeMS: 0, Value: 55}}},
	}
	out := groupSeriesBy(series, "gpu", "avg")
	if len(out) != 2 {
		t.Fatalf("grouped = %+v, want 2 distinct GPUs (not merged by bare index 0)", out)
	}
	byKey := map[string]float64{}
	for _, s := range out {
		byKey[s.Labels["gpu"]] = s.Points[0].Value
	}
	if byKey["GPU-AAA"] != 91 || byKey["GPU-BBB"] != 55 {
		t.Fatalf("byKey = %+v, want keyed by gpu_uuid", byKey)
	}
}

// TestGroupSeriesByGPUFallsBackToNodeIndexWithoutUUID covers an old agent
// whose samples carry no gpu_uuid at all: the fallback key must still keep
// different nodes' same-index cards apart.
func TestGroupSeriesByGPUFallsBackToNodeIndexWithoutUUID(t *testing.T) {
	series := []tsdb.Series{
		{Labels: map[string]string{"node": "node-a", "gpu": "0"}, Points: []tsdb.Point{{TimeMS: 0, Value: 10}}},
		{Labels: map[string]string{"node": "node-b", "gpu": "0"}, Points: []tsdb.Point{{TimeMS: 0, Value: 20}}},
	}
	out := groupSeriesBy(series, "gpu", "avg")
	if len(out) != 2 {
		t.Fatalf("grouped = %+v, want 2 distinct GPUs", out)
	}
	for _, s := range out {
		if s.Labels["gpu"] != "node-a/0" && s.Labels["gpu"] != "node-b/0" {
			t.Fatalf("unexpected fallback grouping key %q", s.Labels["gpu"])
		}
	}
}

func newSeriesTestService(t *testing.T) (*Service, tsdb.Store) {
	t.Helper()
	store := newTestStore(t)
	svc := New(Deps{
		TSDB: store,
		Now:  func() time.Time { return baseTime },
	})
	return svc, store
}

func TestBuildSeriesUnknownMetricIsEmpty(t *testing.T) {
	svc, _ := newSeriesTestService(t)
	out, err := svc.buildSeries(context.Background(), "gpuworks_does_not_exist", "", "avg", time.Hour, 30*time.Second)
	if err != nil {
		t.Fatalf("buildSeries: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %+v, want empty", out)
	}
}

// TestBuildSeriesTooManySeriesIsRejected proves exceeding MaxSeriesCount
// returns Deps.SeriesTooLarge, not a truncated result.
func TestBuildSeriesTooManySeriesIsRejected(t *testing.T) {
	store := newTestStore(t)
	tooLarge := errors.New("too large")
	svc := New(Deps{TSDB: store, Now: func() time.Time { return baseTime }, SeriesTooLarge: tooLarge})
	for i := 0; i < MaxSeriesCount+1; i++ {
		writeSample(t, store, "gpuworks_gpu_utilization", map[string]string{"instance": string(rune('a' + i))}, float64(i), baseTime)
	}
	_, err := svc.buildSeries(context.Background(), "gpuworks_gpu_utilization", "instance", "avg", time.Hour, 30*time.Second)
	if !errors.Is(err, tooLarge) {
		t.Fatalf("err = %v, want the injected SeriesTooLarge sentinel", err)
	}
}

// TestBuildSeriesTooManyPointsIsRejected covers the other cap: one series
// with more than MaxSeriesPoints actual samples (tsdb only returns buckets
// that have data, so the test has to write one real sample per bucket, not
// just ask for a wide range/fine step over sparse data).
func TestBuildSeriesTooManyPointsIsRejected(t *testing.T) {
	store := newTestStore(t)
	tooLarge := errors.New("too large")
	svc := New(Deps{TSDB: store, Now: func() time.Time { return baseTime }, SeriesTooLarge: tooLarge})

	n := MaxSeriesPoints + 10
	samples := make([]tsdb.Sample, n)
	for i := 0; i < n; i++ {
		samples[i] = tsdb.Sample{
			Metric: "gpuworks_gpu_utilization", Labels: map[string]string{"instance": "i1"},
			Value: float64(i), TimeMS: baseTime.Add(-time.Duration(n-i) * time.Second).UnixMilli(),
		}
	}
	if err := store.WriteBatch(samples); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	rng := time.Duration(n+10) * time.Second
	_, err := svc.buildSeries(context.Background(), "gpuworks_gpu_utilization", "", "avg", rng, time.Second)
	if !errors.Is(err, tooLarge) {
		t.Fatalf("err = %v, want the injected SeriesTooLarge sentinel", err)
	}
}

// TestServiceSeriesBasicPath exercises the public Service.Series method
// (not just the unexported buildSeries it wraps), proving the
// monitor.Cached plumbing (single-flight/TTL) sits in front of a working
// computation for the plain, everyday case.
func TestServiceSeriesBasicPath(t *testing.T) {
	svc, store := newSeriesTestService(t)
	writeSample(t, store, "gpuworks_gpu_utilization", map[string]string{"node": "n1", "gpu": "0"}, 42, baseTime)

	out, err := svc.Series(context.Background(), "gpuworks_gpu_utilization", "node", "avg", time.Hour, 30*time.Second)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(out) != 1 || out[0].Labels["node"] != "n1" {
		t.Fatalf("out = %+v, want one series grouped by node n1", out)
	}
	if len(out[0].Points) != 1 || out[0].Points[0][1] != 42 {
		t.Fatalf("points = %+v, want a single [t,42] point", out[0].Points)
	}

	// A second call for the same key must return the cached result rather
	// than erroring or coming back empty.
	again, err := svc.Series(context.Background(), "gpuworks_gpu_utilization", "node", "avg", time.Hour, 30*time.Second)
	if err != nil || len(again) != 1 {
		t.Fatalf("second Series call = %+v, %v", again, err)
	}
}

func TestBuildSeriesFallsBackWhenSeriesTooLargeUnset(t *testing.T) {
	store := newTestStore(t)
	svc := New(Deps{TSDB: store, Now: func() time.Time { return baseTime }})
	for i := 0; i < MaxSeriesCount+1; i++ {
		writeSample(t, store, "gpuworks_gpu_utilization", map[string]string{"instance": string(rune('a' + i))}, float64(i), baseTime)
	}
	_, err := svc.buildSeries(context.Background(), "gpuworks_gpu_utilization", "instance", "avg", time.Hour, 30*time.Second)
	if err == nil {
		t.Fatal("expected an error even with Deps.SeriesTooLarge left nil")
	}
}
