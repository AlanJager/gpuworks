package monitorapi

import (
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// newTestStore opens a real SQLite-backed tsdb.Store in a temp file, the
// same rationale monitor's own testing_test.go documents: exercising the
// actual QueryRange/QueryLatest SQL instead of a hand-rolled fake that could
// silently drift from it.
func newTestStore(t *testing.T) tsdb.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitorapi-test.db")
	store, err := tsdb.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open test tsdb: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func writeSample(t *testing.T, store tsdb.Store, metric string, labels map[string]string, value float64, at time.Time) {
	t.Helper()
	if err := store.WriteBatch([]tsdb.Sample{{Metric: metric, Labels: labels, Value: value, TimeMS: at.UnixMilli()}}); err != nil {
		t.Fatalf("write sample %s: %v", metric, err)
	}
}

var baseTime = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
