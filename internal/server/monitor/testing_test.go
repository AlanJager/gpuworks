package monitor

import (
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// newTestStore opens a real SQLite-backed tsdb.Store in a temp file so
// monitor's tests exercise the actual QueryRange/QueryLatest SQL the server
// runs (bucket alignment, indexed instance/deployment matchers, and so on)
// instead of a hand-rolled fake that could silently drift from it -
// CLAUDE.md's "test fixtures must faithfully reproduce real behavior" rule.
// tsdb is pure Go (glebarez/go-sqlite), so this is cheap enough to do in
// every test that needs a store.
func newTestStore(t testing.TB) tsdb.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor-test.db")
	store, err := tsdb.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open test tsdb: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeSample writes one sample, failing the test on error.
func writeSample(t testing.TB, store tsdb.Store, metric string, labels map[string]string, value float64, at time.Time) {
	t.Helper()
	if err := store.WriteBatch([]tsdb.Sample{{Metric: metric, Labels: labels, Value: value, TimeMS: at.UnixMilli()}}); err != nil {
		t.Fatalf("write sample %s: %v", metric, err)
	}
}

// writeSeries writes one value per tick starting at start and spaced step
// apart, len(values) samples total - the common shape for "this metric held
// steady/ramped for N ticks" test setup.
func writeSeries(t testing.TB, store tsdb.Store, metric string, labels map[string]string, start time.Time, step time.Duration, values []float64) {
	t.Helper()
	for i, v := range values {
		writeSample(t, store, metric, labels, v, start.Add(time.Duration(i)*step))
	}
}

// computeWindowFromStore is window_test.go's usual entry point: fetch one
// deployment's WindowSeries (FetchWindowSeries) and reduce it (ComputeWindow)
// in one call. Tests that inspect the fetch step's own errors/query counts
// call FetchWindowSeries/ComputeWindow directly instead.
func computeWindowFromStore(t *testing.T, store tsdb.Store, deploymentID uint, instances []InstanceRef, now time.Time, span time.Duration) Window {
	t.Helper()
	series, err := FetchWindowSeries(store, deploymentID, instances, now, now.Add(-span), WindowStep.Milliseconds())
	if err != nil {
		t.Fatalf("FetchWindowSeries: %v", err)
	}
	return ComputeWindow(series, WindowParams{Now: now, Span: span})
}

// erroringStore wraps a real tsdb.Store and fails QueryRange for one
// specific metric, so tests can prove BuildCluster/BuildService propagate a
// TSDB error instead of quietly returning a zeroed snapshot (F12).
type erroringStore struct {
	tsdb.Store
	failMetric string
	err        error
}

func (e *erroringStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	if metric == e.failMetric {
		return nil, e.err
	}
	return e.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

// recordedQuery is one QueryRange call queryRecordingStore observed.
type recordedQuery struct {
	metric   string
	matchers map[string]string
	stepMS   int64
}

// recordedLatest is one QueryLatest call queryRecordingStore observed - kept
// separate from recordedQuery/calls (a different method, a different cost
// shape) so a test asserting "QueryRange count did not change" is never
// accidentally satisfied or broken by BuildCluster's own QueryLatest-based
// rng-presence probes (see monitor.rangeDeploymentPresence).
type recordedLatest struct {
	metric  string
	sinceMS int64
}

// queryRecordingStore wraps a real tsdb.Store and records every QueryRange
// and QueryLatest call, so tests can assert on query counts and shapes: how
// many ran at all, whether any used an "instance" matcher (a per-instance
// fallback query), or what step (and so which span) a query used.
type queryRecordingStore struct {
	tsdb.Store
	calls       []recordedQuery
	latestCalls []recordedLatest
}

func (s *queryRecordingStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	s.calls = append(s.calls, recordedQuery{metric: metric, matchers: matchers, stepMS: stepMS})
	return s.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

func (s *queryRecordingStore) QueryLatest(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]tsdb.LatestPoint, error) {
	s.latestCalls = append(s.latestCalls, recordedLatest{metric: metric, sinceMS: sinceMS})
	return s.Store.QueryLatest(metric, matchers, sinceMS, asOfMS...)
}
