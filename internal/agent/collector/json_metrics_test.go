package collector

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The mapping the built-in Strata gw6 template declares, as resolved.
var strataRules = []proto.MetricRule{
	{Source: "live.state!=idle", Series: "gpuworks_infer_requests_running"},
	{Source: "live.queued", Series: "gpuworks_infer_requests_waiting"},
	{Source: "live.generated", Series: "gpuworks_infer_generation_tokens_total"},
	{Source: "live.prompt_tokens", Series: "gpuworks_infer_prompt_tokens_total"},
	{Source: "totals.drafts_accepted", Series: "gpuworks_infer_spec_accepted_tokens_total"},
	{Source: "totals.drafts_offered", Series: "gpuworks_infer_spec_draft_tokens_total"},
	{Source: "totals.output_tokens", Series: "gpuworks_infer_generation_tokens_total"},
	{Source: "totals.prompt_tokens", Series: "gpuworks_infer_prompt_tokens_total"},
	{Source: "totals.requests", Series: "gpuworks_infer_requests_success_total"},
}

func strataInstance(rules []proto.MetricRule) RunningInstance {
	return RunningInstance{UUID: "strata-1", Engine: "callable", DeploymentID: 15, Observation: &proto.InstanceObservation{MetricFormat: "json", MetricRules: rules}}
}

// A real Strata 0.1.38 /metrics document, captured while it generated: the
// running request counts, and its tokens so far add to the finished totals.
func TestJSONMetricsReadARealStrataDocument(t *testing.T) {
	data, err := os.ReadFile("../../inferencemetrics/testdata/strata-gw6-metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	got := sampleIndex(parseJSONMetrics(strings.NewReader(string(data)), strataInstance(strataRules), "node-1", 7, slog.Default(), nil))
	want := map[string]float64{
		"gpuworks_infer_requests_running":           1,
		"gpuworks_infer_requests_waiting":           0,
		"gpuworks_infer_requests_success_total":     13,
		"gpuworks_infer_prompt_tokens_total":        2548 + 50,
		"gpuworks_infer_generation_tokens_total":    3356 + 1971,
		"gpuworks_infer_spec_draft_tokens_total":    2645,
		"gpuworks_infer_spec_accepted_tokens_total": 1578,
	}
	if len(got) != len(want) {
		t.Fatalf("samples = %v", got)
	}
	for metric, value := range want {
		sample := got[metric]
		if sample.Value != value || sample.TimeMS != 7 || sample.Labels["instance"] != "strata-1" || sample.Labels["deployment"] != "15" || sample.Labels["engine"] != "callable" || sample.Labels["node"] != "node-1" {
			t.Errorf("%s = %+v, want %v", metric, sample, value)
		}
	}
}

func TestJSONMetricsPaths(t *testing.T) {
	doc := `{"live": {"state": "idle", "queued": null, "busy": true, "n": 3}, "totals": {"a_ms": 2500, "b": 4}, "list": [1, 2]}`
	rules := []proto.MetricRule{
		{Source: "live.state", Series: "gpuworks_infer_requests_running"},  // a string: no sample
		{Source: "live.queued", Series: "gpuworks_infer_requests_waiting"}, // null: no sample
		{Source: "live.busy", Series: "gpuworks_infer_spec_accept_rate"},   // a boolean reads 1
		{Source: "totals.a_ms", Series: "gpuworks_infer_prompt_tokens_total", Scale: 0.001},
		{Source: "totals.b", Series: "gpuworks_infer_prompt_tokens_total"}, // two paths, one series: summed
		{Source: "list.0", Series: "gpuworks_infer_kv_hits_total"},         // not an object: no sample
		{Source: "missing.path", Series: "gpuworks_infer_kv_queries_total"},
		{Source: "live..n", Series: "gpuworks_infer_preemptions_total"}, // not a path: skipped
	}
	got := sampleIndex(parseJSONMetrics(strings.NewReader(doc), strataInstance(rules), "n", 1, nil, nil))
	if len(got) != 2 || got["gpuworks_infer_spec_accept_rate"].Value != 1 || got["gpuworks_infer_prompt_tokens_total"].Value != 6.5 {
		t.Fatalf("samples = %v", got)
	}
	// A comparison reads a string as 1 or 0, and anything else as nothing.
	compared := []proto.MetricRule{
		{Source: "live.state==idle", Series: "gpuworks_infer_requests_running"},
		{Source: "live.state!=idle", Series: "gpuworks_infer_requests_waiting"},
		{Source: "live.queued!=idle", Series: "gpuworks_infer_spec_accept_rate"},   // null
		{Source: "live.n==3", Series: "gpuworks_infer_spec_accept_length"},         // a number
		{Source: "live.busy!=false", Series: "gpuworks_infer_kv_hit_rate_instant"}, // a boolean
		{Source: "live.gone!=idle", Series: "gpuworks_infer_kv_free_tokens"},       // absent
	}
	got = sampleIndex(parseJSONMetrics(strings.NewReader(doc), strataInstance(compared), "n", 1, nil, nil))
	if len(got) != 2 || got["gpuworks_infer_requests_running"].Value != 1 || got["gpuworks_infer_requests_waiting"].Value != 0 {
		t.Fatalf("compared = %v", got)
	}
	// Derived metrics apply to JSON values as to Prometheus ones.
	kv := []proto.MetricRule{
		{Source: "pool.size", Series: "gpuworks_infer_kv_capacity_tokens"},
		{Source: "pool.free", Series: "gpuworks_infer_kv_free_tokens"},
	}
	derived := sampleIndex(parseJSONMetrics(strings.NewReader(`{"pool": {"size": 100, "free": 25}}`), strataInstance(kv), "n", 1, nil, nil))
	if derived["gpuworks_infer_gpu_cache_usage_percent"].Value != 0.75 {
		t.Fatalf("derived = %v", derived)
	}
}

func TestJSONMetricsRefuseWhatIsNotOneWholeDocument(t *testing.T) {
	for name, body := range map[string]string{
		"prometheus text": "live_queued 3\n",
		"truncated":       `{"live": {"queued": 3`,
		"over a mebibyte": `{"live": {"queued": 3}, "pad": "` + strings.Repeat("x", oneMiB) + `"}`,
		"empty":           "",
	} {
		if samples := parseJSONMetrics(strings.NewReader(body), strataInstance(strataRules), "n", 1, nil, nil); len(samples) != 0 {
			t.Fatalf("%s: samples = %v", name, samples)
		}
	}
}

func TestInferSamplesReadsAJSONEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"live": {"queued": 2}}`))
	})}
	go srv.Serve(listener)
	defer srv.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	in := strataInstance([]proto.MetricRule{{Source: "live.queued", Series: "gpuworks_infer_requests_waiting"}})
	in.APIPort = port
	c := New("node-1", nil, func() []RunningInstance { return []RunningInstance{in} }, nil, slog.Default())
	samples := c.inferSamples(context.Background(), 1)
	if len(samples) != 1 || samples[0].Metric != "gpuworks_infer_requests_waiting" || samples[0].Value != 2 {
		t.Fatalf("samples = %+v", samples)
	}
	// The same document read as Prometheus text gives nothing.
	in.Observation.MetricFormat = ""
	if samples := c.inferSamples(context.Background(), 1); len(samples) != 0 {
		t.Fatalf("prometheus read of json = %+v", samples)
	}
}

// strataReading renders a Strata document of one request in flight
// (generated < 0 is idle) on top of finished totals.
func strataReading(totalOutput, generated int) string {
	if generated < 0 {
		return fmt.Sprintf(`{"live": {"state": "idle", "queued": 0, "generated": null, "prompt_tokens": null}, "totals": {"requests": 1, "output_tokens": %d, "prompt_tokens": 10}}`, totalOutput)
	}
	return fmt.Sprintf(`{"live": {"state": "generating", "queued": 0, "generated": %d, "prompt_tokens": 68}, "totals": {"requests": 1, "output_tokens": %d, "prompt_tokens": 10}}`, generated, totalOutput)
}

func generationReadings(t *testing.T, hold *counterHold, instance RunningInstance, readings []string) []float64 {
	t.Helper()
	out := make([]float64, 0, len(readings))
	for _, reading := range readings {
		sample, ok := sampleIndex(parseJSONMetrics(strings.NewReader(reading), instance, "n", 1, nil, hold))["gpuworks_infer_generation_tokens_total"]
		if !ok {
			t.Fatalf("no generation counter in %s", reading)
		}
		out = append(out, sample.Value)
	}
	return out
}

// The sequence Strata reported through one request
// (evidence/strata-gw6-live-during-request.jsonl): the summed counter climbs
// while it runs and lands on the finished total, so throughput spreads over
// the request instead of arriving at its end.
func TestJSONCounterClimbsThroughARequest(t *testing.T) {
	var hold counterHold
	got := generationReadings(t, &hold, strataInstance(strataRules), []string{
		strataReading(16092, -1), strataReading(16092, 20), strataReading(16092, 90), strataReading(16092, 177),
		strataReading(16092, 353), strataReading(16092, 689), strataReading(16808, -1),
	})
	want := []float64{16092, 16112, 16182, 16269, 16445, 16781, 16808}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("readings = %v, want %v", got, want)
	}
}

// A request cancelled before it reaches the totals takes its in-flight tokens
// back out of the sum; the counter holds instead of falling, which the
// monitor would read as a reset and turn into a spike of every token so far.
// A deep fall - the engine restarted - still passes through.
func TestJSONCounterHoldsAShallowFallAndPassesAReset(t *testing.T) {
	var hold counterHold
	instance := strataInstance(strataRules)
	got := generationReadings(t, &hold, instance, []string{
		strataReading(1000, 400), // in flight
		strataReading(1000, -1),  // cancelled: not added to totals
		strataReading(1000, 100), // the next request, still under the held reading
		strataReading(1000, 600), // past it
		strataReading(1600, -1),  // finished
		strataReading(0, 50),     // the engine restarted
	})
	want := []float64{1400, 1400, 1400, 1600, 1600, 50}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("readings = %v, want %v", got, want)
	}
	// Gauges are never held: the running request count falls back to 0.
	running := sampleIndex(parseJSONMetrics(strings.NewReader(strataReading(1600, -1)), instance, "n", 1, nil, &hold))["gpuworks_infer_requests_running"]
	if running.Value != 0 {
		t.Fatalf("running = %v", running)
	}
	// Another instance has its own readings, and a stopped one is forgotten.
	other := instance
	other.UUID = "strata-2"
	if got := generationReadings(t, &hold, other, []string{strataReading(10, -1)}); got[0] != 10 {
		t.Fatalf("other instance = %v", got)
	}
	hold.retain(map[string]struct{}{"strata-2": {}})
	if got := generationReadings(t, &hold, instance, []string{strataReading(30, -1)}); got[0] != 30 {
		t.Fatalf("after retain = %v", got)
	}
}
