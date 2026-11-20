package proxy

import (
	"testing"
	"time"
)

// The hit-rate denominator counts only the replies that stated a cache
// figure: a reply with usage but no figure is unknown, not a miss, and a
// reply without usage says nothing.
func TestCacheReportedPromptCountsOnlyRepliesThatStatedACacheFigure(t *testing.T) {
	var r responseCounters
	for _, stats := range []responseStats{
		{observed: true, usage: true, prompt: 100, cached: 60, cachedKnown: true},
		{observed: true, usage: true, prompt: 50, cached: 0, cachedKnown: true}, // a stated zero is a miss
		{observed: true, usage: true, prompt: 400},                              // usage without the figure
		{observed: true}, // no usage at all
	} {
		r.record(time.Second, stats, nil)
	}
	samples := r.samples("7", 1000)
	dep := map[string]string{"deployment": "7"}
	for metric, want := range map[string]float64{
		metricPromptTokens:  550,
		metricCachedTokens:  60,
		metricCacheReported: 150,
		metricUsageMissing:  1,
	} {
		if got, ok := sampleValue(samples, metric, dep); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", metric, got, ok, want)
		}
	}
}

// A deployment that serves no usage-reporting operation (images, audio)
// writes no cache series at all.
func TestNoCacheSeriesWithoutUsage(t *testing.T) {
	var r responseCounters
	r.record(time.Second, responseStats{}, nil)
	for _, s := range r.samples("7", 1000) {
		if s.Metric == metricCacheReported || s.Metric == metricCachedTokens {
			t.Errorf("unexpected series %s", s.Metric)
		}
	}
}
