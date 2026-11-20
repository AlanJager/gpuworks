package collector

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
)

// serveMetrics answers /metrics on a loopback port with one in-flight gauge and
// reports the port and how many requests it took.
func serveMetrics(t *testing.T, path string, inflight string) (port int, hits func() int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	count := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("custom_inflight " + inflight + "\n"))
	})}
	go srv.Serve(listener)
	t.Cleanup(func() { _ = srv.Close() })
	return listener.Addr().(*net.TCPAddr).Port, func() int { mu.Lock(); defer mu.Unlock(); return count }
}

// An instance launched with a metrics port of its own is scraped there, never
// on the API port, which (for an engine that owns it completely) has no
// /metrics; without one the API port is scraped as before.
func TestInferSamplesScrapesTheSeparateMetricsPort(t *testing.T) {
	rule := map[string]string{"custom_inflight": "gpuworks_infer_requests_running"}
	apiPort, apiHits := serveMetrics(t, "/nothing-here", "1")
	metricsPort, metricsHits := serveMetrics(t, "/custom", "7")
	c := New("node-1", nil, func() []RunningInstance {
		return []RunningInstance{{UUID: "inst-1", APIPort: apiPort, MetricsPort: metricsPort, MetricPath: "/custom", Engine: "custom", MetricMap: rule}}
	}, nil, slog.Default())
	samples := c.inferSamples(context.Background(), 123)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_running" || samples[0].Value != 7 {
		t.Fatalf("samples=%+v", samples)
	}
	if apiHits() != 0 || metricsHits() != 1 {
		t.Fatalf("API port took %d requests, metrics port %d; want 0 and 1", apiHits(), metricsHits())
	}

	onAPI := New("node-1", nil, func() []RunningInstance {
		return []RunningInstance{{UUID: "inst-2", APIPort: metricsPort, MetricPath: "/custom", Engine: "custom", MetricMap: rule}}
	}, nil, slog.Default())
	if samples := onAPI.inferSamples(context.Background(), 123); len(samples) != 1 || samples[0].Value != 7 {
		t.Fatalf("without a metrics port: samples=%+v", samples)
	}
}

func TestScrapePortPrefersTheMetricsPort(t *testing.T) {
	for _, tc := range []struct {
		in   RunningInstance
		want int
	}{
		{RunningInstance{APIPort: 30000}, 30000},
		{RunningInstance{APIPort: 30000, MetricsPort: 30001}, 30001},
		{RunningInstance{APIPort: 0, MetricsPort: 0}, 0},
	} {
		if got := tc.in.scrapePort(); got != tc.want {
			t.Errorf("%+v scrapes %d, want %d", tc.in, got, tc.want)
		}
	}
}
