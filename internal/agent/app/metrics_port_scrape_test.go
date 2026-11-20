package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/collector"
	"gpuworks.ai/gpuworks/internal/proto"
)

// metricsHost is a loopback HTTP server that serves /custom (a Prometheus
// exposition with one in-flight gauge) when it is meant to, 404 otherwise, and
// counts every request it takes by path.
type metricsHost struct {
	port int
	mu   sync.Mutex
	hits map[string]int
}

func newMetricsHost(t *testing.T, servesMetrics bool, inflight int) *metricsHost {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &metricsHost{port: listener.Addr().(*net.TCPAddr).Port, hits: map[string]int{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits[r.URL.Path]++
		h.mu.Unlock()
		if !servesMetrics || r.URL.Path != "/custom" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "custom_inflight %d\n", inflight)
	})}
	go srv.Serve(listener)
	t.Cleanup(func() { _ = srv.Close() })
	return h
}

func (h *metricsHost) taken(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[path]
}

func (h *metricsHost) seen() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for path, count := range h.hits {
		out[path] = count
	}
	return out
}

// desiredFromWire decodes a desired state the way the agent receives it: from
// JSON, so a key the server did not write is absent rather than zero by
// construction.
func desiredFromWire(t *testing.T, raw string) proto.InstanceDesiredState {
	t.Helper()
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func wireDesired(uuid string, apiPort, metricsPort int, extra string) string {
	metrics := ""
	ports := fmt.Sprintf("[%d]", apiPort)
	if metricsPort > 0 {
		metrics = fmt.Sprintf(`,"metrics_port":%d`, metricsPort)
		ports = fmt.Sprintf("[%d,%d]", apiPort, metricsPort)
	}
	return fmt.Sprintf(`{"launch":{"argv":[],"model_mount":""},"uuid":%q,"deployment_id":7,"engine":"custom","desired_state":"running","image":"img","gpu_indexes":[0],"host_network":true,"ports":%s,"api_port":%d%s,"metric_path":"/custom","metric_map":{"custom_inflight":"gpuworks_infer_requests_running"}%s}`,
		uuid, ports, apiPort, metrics, extra)
}

// The agent's collector, fed by a real desired state as the server writes it,
// scrapes the port the instance was launched with for metrics and never the
// API port; an instance without one (a template that did not declare the port,
// or a server that predates the field and so writes no key) is scraped on the
// API port as before; a headless rank is not scraped at all.
func TestCollectorScrapesTheMetricsPortFromTheDesiredStateOnTheWire(t *testing.T) {
	ownAPI, ownMetrics := newMetricsHost(t, false, 0), newMetricsHost(t, true, 7)
	plain := newMetricsHost(t, true, 3)
	// What an old server sends: no metrics_port key, the API port scraped.
	legacy := newMetricsHost(t, true, 5)
	headless := newMetricsHost(t, true, 9)

	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), &imagePullTestDriver{}, t.TempDir(), "", metricsOnlyProber{}, nil)
	if n.engine == nil {
		t.Fatal("test requires a node with a container engine")
	}
	n.engine.SetDesiredFull(t.Context(), []proto.InstanceDesiredState{
		desiredFromWire(t, wireDesired("own-port", ownAPI.port, ownMetrics.port, "")),
		desiredFromWire(t, wireDesired("api-port", plain.port, 0, "")),
		desiredFromWire(t, wireDesired("old-server", legacy.port, 0, "")),
		desiredFromWire(t, wireDesired("headless", headless.port, 0, `,"headless":true`)),
	})
	n.collectorRunner = func(ctx context.Context, col *collector.Collector) { col.RunOnce(ctx) }
	batches := make(chan proto.MetricBatch, 1)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TMetrics {
			select {
			case batches <- body.(proto.MetricBatch):
			default:
			}
		}
		return nil
	})
	n.StartBackground(t.Context(), "node-metrics-port")
	var batch proto.MetricBatch
	select {
	case batch = <-batches:
	case <-time.After(5 * time.Second):
		t.Fatal("the collector never emitted")
	}
	inflight := map[string]float64{}
	for _, sample := range batch.Samples {
		if sample.Metric == "gpuworks_infer_requests_running" {
			inflight[sample.Labels["instance"]] = sample.Value
		}
	}
	want := map[string]float64{"own-port": 7, "api-port": 3, "old-server": 5}
	if len(inflight) != len(want) || inflight["own-port"] != want["own-port"] || inflight["api-port"] != want["api-port"] || inflight["old-server"] != want["old-server"] {
		t.Fatalf("inference samples by instance = %v, want %v", inflight, want)
	}
	if len(ownAPI.seen()) != 0 || ownMetrics.taken("/custom") != 1 {
		t.Fatalf("the instance with a metrics port: API port took %v, metrics port %v", ownAPI.seen(), ownMetrics.seen())
	}
	if plain.taken("/custom") != 1 || legacy.taken("/custom") != 1 {
		t.Fatalf("API-port instances took %v and %v", plain.seen(), legacy.seen())
	}
	if len(headless.seen()) != 0 {
		t.Fatalf("a headless rank was scraped: %v", headless.seen())
	}
}

// The metrics port is part of the instance's launch, so nothing that reaches
// an agent for a running instance moves it: a full sync after a reconnect
// carries it again, and the server's observation refresh (the one thing it
// may change in place) swaps how the instance is read, not where.
func TestMetricsPortSurvivesReconnectSyncAndObservationRefresh(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), &imagePullTestDriver{}, t.TempDir(), "", metricsOnlyProber{}, nil)
	if n.engine == nil {
		t.Fatal("test requires a node with a container engine")
	}
	spec := desiredFromWire(t, wireDesired("own-port", 30000, 30001, ""))
	ports := func() [2]int {
		t.Helper()
		var found [2]int
		for _, in := range n.runningInstances() {
			if in.UUID == "own-port" {
				found = [2]int{in.APIPort, in.MetricsPort}
			}
		}
		return found
	}
	n.engine.SetDesiredFull(t.Context(), []proto.InstanceDesiredState{spec})
	if got := ports(); got != [2]int{30000, 30001} {
		t.Fatalf("first sync: %v", got)
	}
	// The connection drops and comes back: the server sends the full state again.
	n.engine.SetDesiredFull(t.Context(), []proto.InstanceDesiredState{spec})
	if got := ports(); got != [2]int{30000, 30001} {
		t.Fatalf("after the reconnect sync: %v", got)
	}
	n.engine.UpdateObservations([]proto.ObservationUpdate{{
		UUID: "own-port", MetricPath: "/m2", MetricMap: map[string]string{"x": "gpuworks_infer_requests_running"},
		Observation: &proto.InstanceObservation{MetricPath: "/m2"},
	}})
	if got := ports(); got != [2]int{30000, 30001} {
		t.Fatalf("after an observation refresh: %v", got)
	}
	for _, in := range n.runningInstances() {
		if in.UUID == "own-port" && in.MetricPath != "/m2" {
			t.Fatalf("the refresh did not apply: %+v", in)
		}
	}
}
