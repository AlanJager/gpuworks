package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newMetricsQueryHandler builds a handler whose MetricsQuery dependency only
// records the parsed window so tests can assert handler-side parameter rules.
func newMetricsQueryHandler(recorder func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error)) *apiHandlers {
	return &apiHandlers{d: Deps{
		MetricsQuery: func(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
			return recorder(metric, startMS, endMS, stepMS)
		},
	}}
}

func TestMetricsQueryStartEndTakePrecedenceOverRange(t *testing.T) {
	var gotStart, gotEnd, gotStep int64
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		gotStart, gotEnd, gotStep = startMS, endMS, stepMS
		return []dto.MonitorSeries{}, nil
	})
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&range=1h&start=1000&end=2000&step=30s", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotStart != 1000 || gotEnd != 2000 {
		t.Fatalf("window = [%d, %d], want [1000, 2000]", gotStart, gotEnd)
	}
	if gotStep != 30_000 {
		t.Fatalf("stepMS = %d, want 30000", gotStep)
	}
}

func TestMetricsQueryRangeOnlyStaysRelativeNow(t *testing.T) {
	var gotStart, gotEnd int64
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		gotStart, gotEnd = startMS, endMS
		return []dto.MonitorSeries{}, nil
	})
	before := time.Now().UTC().Add(-15 * time.Second)
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&range=15m&step=10s", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	after := time.Now().UTC()
	if gotEnd := time.UnixMilli(gotEnd); gotEnd.Before(before) || gotEnd.After(after) {
		t.Fatalf("end = %v, want ~now (%v..%v)", gotEnd, before, after)
	}
	if delta := gotEnd - gotStart; delta != 15*60*1000 {
		t.Fatalf("window = %d ms, want 15m", delta)
	}
}

func TestMetricsQueryEndOnlyDefaultsStartToRange(t *testing.T) {
	var gotStart, gotEnd int64
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		gotStart, gotEnd = startMS, endMS
		return []dto.MonitorSeries{}, nil
	})
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&range=1h&end=5000", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotEnd != 5000 {
		t.Fatalf("end = %d, want 5000", gotEnd)
	}
	if delta := gotEnd - gotStart; delta != 60*60*1000 {
		t.Fatalf("window = %d ms, want 1h back from end", delta)
	}
}

func TestMetricsQueryStartNotBeforeEndIsRejected(t *testing.T) {
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		t.Fatal("MetricsQuery must not be called for an inverted window")
		return nil, nil
	})
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&start=2000&end=1000", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// start == end is equally useless as a range query.
	req = httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&start=1000&end=1000", nil)
	rec = httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMetricsQueryUnparsableBoundsFallBackToRange(t *testing.T) {
	var gotStart, gotEnd int64
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		gotStart, gotEnd = startMS, endMS
		return []dto.MonitorSeries{}, nil
	})
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=m&range=1h&start=abc&end=-5", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if delta := gotEnd - gotStart; delta != 60*60*1000 {
		t.Fatalf("window = %d ms, want the 1h range fallback", delta)
	}
}

// Ported to the v2 filter signature: from/to windows, the 5000-row ceiling,
// and the truncation header. hide_metrics is gone — the agent drops scrape
// noise before shipping, so the server never sees those rows.
func TestLogsSearchPlumbsTimeBounds(t *testing.T) {
	for _, test := range []struct {
		name      string
		query     string
		wantFrom  int64
		wantTo    int64
		wantLimit int
	}{
		{"no bounds", "q=boom&instance=i&limit=50", 0, 0, 50},
		{"bounds pass through", "q=boom&from=100&to=900&limit=1", 100, 900, 1},
		{"limit clamp default", "q=boom", 0, 0, 200},
		{"limit clamps to the ceiling", "q=boom&limit=99999", 0, 0, 5000},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got LogsFilter
			handler := &apiHandlers{d: Deps{
				LogsSearch: func(f LogsFilter) (LogsPage, error) {
					got = f
					return LogsPage{Items: []LogEntry{}}, nil
				},
			}}
			req := httptest.NewRequest("GET", "/api/v1/logs/search?"+test.query, nil)
			rec := httptest.NewRecorder()
			handler.logsSearch(rec, req)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			if got.Query != "boom" {
				t.Fatalf("q = %q", got.Query)
			}
			if test.name == "no bounds" && got.Instance != "i" {
				t.Fatalf("instance = %q, want i", got.Instance)
			}
			if got.FromMS != test.wantFrom || got.ToMS != test.wantTo {
				t.Fatalf("bounds = [%d, %d], want [%d, %d]", got.FromMS, got.ToMS, test.wantFrom, test.wantTo)
			}
			// Truncation is now the store's own call (logstore.Result
			// .Truncated), so the handler passes the requested limit straight
			// through without probing it itself.
			if got.Limit != test.wantLimit {
				t.Fatalf("limit = %d, want %d", got.Limit, test.wantLimit)
			}
			if rec.Header().Get("X-Logs-Truncated") != "" {
				t.Fatal("a response within the limit must not carry the truncation header")
			}
		})
	}
}

func TestLogsSearchRejectsInvertedWindow(t *testing.T) {
	handler := &apiHandlers{d: Deps{LogsSearch: func(LogsFilter) (LogsPage, error) {
		return LogsPage{}, nil
	}}}
	req := httptest.NewRequest("GET", "/api/v1/logs/search?q=boom&from=900&to=100", nil)
	rec := httptest.NewRecorder()
	handler.logsSearch(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 for from >= to", rec.Code)
	}
}

func TestLogsSearchFlagsTruncation(t *testing.T) {
	handler := &apiHandlers{d: Deps{
		LogsSearch: func(f LogsFilter) (LogsPage, error) {
			// Truncation is the store's own call now (logstore.Result
			// .Truncated) - simulate a full page with more rows behind it.
			return LogsPage{Items: make([]LogEntry, f.Limit), Truncated: true}, nil
		},
	}}
	req := httptest.NewRequest("GET", "/api/v1/logs/search?q=boom&limit=50", nil)
	rec := httptest.NewRecorder()
	handler.logsSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Logs-Truncated") != "1" {
		t.Fatal("truncated response must carry X-Logs-Truncated: 1")
	}
}

func TestMetricsQueryPassesLinkAndRoleMatchers(t *testing.T) {
	var got map[string]string
	handler := &apiHandlers{d: Deps{MetricsQuery: func(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		got = matchers
		return []dto.MonitorSeries{}, nil
	}}}
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=gpuworks_node_rdma_rx_bytes_per_sec&node=n1&iface=bond0&hca=mlx5_0:1&role=storage_sync&bogus=x", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	want := map[string]string{"node": "n1", "iface": "bond0", "hca": "mlx5_0:1", "role": "storage_sync"}
	if rec.Code != 200 || len(got) != len(want) {
		t.Fatalf("status %d, matchers = %v, want %v", rec.Code, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("matchers = %v, want %v", got, want)
		}
	}
}

// TestMetricsQueryAnswersListEnvelopeWithPointPairs pins the literal wire
// JSON: the series list in the {items, total} envelope, each point a
// [unix ms, value] pair, and an empty result as "items": [] rather than null.
func TestMetricsQueryAnswersListEnvelopeWithPointPairs(t *testing.T) {
	handler := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		return []dto.MonitorSeries{
			{Labels: map[string]string{"node": "gpu3", "gpu": "0"}, Points: []dto.Point{{1000, 12.5}, {2000, 13}}},
			{Labels: map[string]string{}, Points: []dto.Point{}},
		}, nil
	})
	req := httptest.NewRequest("GET", "/api/v1/metrics/query?metric=gpu_util&start=1000&end=3000&step=1s", nil)
	rec := httptest.NewRecorder()
	handler.metricsQuery(rec, req)
	want := `{"items":[{"labels":{"gpu":"0","node":"gpu3"},"points":[[1000,12.5],[2000,13]]},{"labels":{},"points":[]}],"total":2}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want {
		t.Fatalf("metrics query = %d\n got %s\nwant %s", rec.Code, got, want)
	}

	empty := newMetricsQueryHandler(func(metric string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
		return nil, nil
	})
	rec = httptest.NewRecorder()
	empty.metricsQuery(rec, httptest.NewRequest("GET", "/api/v1/metrics/query?metric=gpu_util", nil))
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != `{"items":[],"total":0}` {
		t.Fatalf("empty metrics query = %d %s", rec.Code, got)
	}

	unwired := &apiHandlers{}
	rec = httptest.NewRecorder()
	unwired.metricsQuery(rec, httptest.NewRequest("GET", "/api/v1/metrics/query?metric=gpu_util", nil))
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != `{"items":[],"total":0}` {
		t.Fatalf("unwired metrics query = %d %s", rec.Code, got)
	}
}

// No data under a name the catalog does not define is a misspelt metric: the
// answer says so and names the closest catalogued series. A catalogued name
// with no data is an empty window, and data under any name is answered.
func TestMetricsQueryUnknownMetricNamesTheClosestSeries(t *testing.T) {
	catalog := []dto.MonitorMetricSeries{
		{Series: "gpuworks_gpu_utilization", Source: "gpu"},
		{Series: "gpuworks_gpu_memory_used_bytes", Source: "gpu"},
		{Series: "gpuworks_node_cpu_busy_percent", Source: "node"},
		{Series: "gpuworks_dgpu_sm_util", Source: "dgpu"},
	}
	query := func(metric string, data []dto.MonitorSeries) *httptest.ResponseRecorder {
		h := &apiHandlers{d: Deps{
			Monitor: &fakeMonitorService{catalog: catalog},
			MetricsQuery: func(string, map[string]string, int64, int64, int64) ([]dto.MonitorSeries, error) {
				return data, nil
			},
		}}
		rec := httptest.NewRecorder()
		h.metricsQuery(rec, httptest.NewRequest("GET", "/api/v1/metrics/query?metric="+metric, nil))
		return rec
	}

	rec := query("gpu_utilization", nil)
	want := `{"code":"METRIC_UNKNOWN","error":"unknown metric gpu_utilization; similar: gpuworks_gpu_utilization, gpuworks_gpu_memory_used_bytes","params":{"metric":"gpu_utilization","similar":"gpuworks_gpu_utilization, gpuworks_gpu_memory_used_bytes"}}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 400 || got != want {
		t.Fatalf("unknown metric = %d\n got %s\nwant %s", rec.Code, got, want)
	}
	// Nothing in common: no suggestions, still a refusal.
	if rec := query("tokens_per_watt", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"similar":""`) {
		t.Fatalf("unrelated metric = %d %s", rec.Code, rec.Body.String())
	}
	if rec := query("gpuworks_gpu_utilization", nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"items":[],"total":0}` {
		t.Fatalf("catalogued metric without data = %d %s", rec.Code, rec.Body.String())
	}
	data := []dto.MonitorSeries{{Labels: map[string]string{"node": "n1"}, Points: []dto.Point{{1, 2}}}}
	if rec := query("gpuworks_custom_series", data); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("uncatalogued metric with data = %d %s", rec.Code, rec.Body.String())
	}
}

// Among names sharing the same words, the one with fewest words besides
// comes first: gpu_utilization means gpuworks_gpu_utilization, not the
// memory bandwidth one.
func TestSimilarMetricsPrefersTheTightestMatch(t *testing.T) {
	catalog := []dto.MonitorMetricSeries{
		{Series: "gpuworks_gpu_memory_bandwidth_utilization"},
		{Series: "gpuworks_gpu_utilization"},
		{Series: "gpuworks_gpu_fan_speed_percent"},
	}
	similar, known := similarMetrics(catalog, "gpu_utilization")
	if known || len(similar) != 3 || similar[0] != "gpuworks_gpu_utilization" || similar[1] != "gpuworks_gpu_memory_bandwidth_utilization" {
		t.Fatalf("similar = %v known = %v", similar, known)
	}
}

// A series carrying a node gains that node's display name, read once for the
// whole answer: a chart of the fleet's cards is read by node name, not UUID.
// A deleted node is named by the name it left, marked so; a node the store
// does not know keeps its labels as they were.
func TestMetricsQueryNamesTheNodes(t *testing.T) {
	st, err := store.Open("file:metrics_query_node_names?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("node-a", store.HashToken("t"), "host-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-deleted", store.HashToken("d"), "host-d"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode("node-deleted"); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st, MetricsQuery: func(string, map[string]string, int64, int64, int64) ([]dto.MonitorSeries, error) {
		return []dto.MonitorSeries{
			{Labels: map[string]string{"node": "node-a", "gpu": "1"}, Points: []dto.Point{{1000, 1}}},
			{Labels: map[string]string{"node": "node-gone", "gpu": "0"}, Points: []dto.Point{{1000, 2}}},
			{Labels: map[string]string{"node": "node-deleted", "gpu": "0"}, Points: []dto.Point{{1000, 3}}},
		}, nil
	}}}
	rec := httptest.NewRecorder()
	h.metricsQuery(rec, httptest.NewRequest("GET", "/api/v1/metrics/query?metric=gpuworks_gpu_utilization&range=1h", nil))
	want := `{"items":[{"labels":{"gpu":"1","node":"node-a","node_name":"host-a"},"points":[[1000,1]]},{"labels":{"gpu":"0","node":"node-gone"},"points":[[1000,2]]},{"labels":{"gpu":"0","node":"node-deleted","node_deleted":"true","node_name":"host-d"},"points":[[1000,3]]}],"total":3}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want {
		t.Fatalf("metrics query = %d\n got %s\nwant %s", rec.Code, got, want)
	}
}
