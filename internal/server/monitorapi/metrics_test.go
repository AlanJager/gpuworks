package monitorapi

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/platformmetrics"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

var updateFixture = flag.Bool("update", false, "rewrite ui/tests/fixtures/monitor-metrics.json from Service.Metrics()")

func TestMetricVocabularyListsEveryStoredSeries(t *testing.T) {
	got := map[string]struct {
		source, kind, unit string
		quantiles          bool
	}{}
	for _, m := range (&Service{}).Metrics() {
		if _, dup := got[m.Series]; dup {
			t.Fatalf("%s listed twice", m.Series)
		}
		got[m.Series] = struct {
			source, kind, unit string
			quantiles          bool
		}{m.Source, m.Kind, m.Unit, m.Quantiles}
	}
	// A histogram is its quantile gauges plus _sum and _count.
	if ttft := got["gpuworks_infer_ttft_seconds"]; !ttft.quantiles || ttft.source != "engine" || ttft.unit != "seconds" {
		t.Fatalf("engine ttft = %+v", ttft)
	}
	if sum, count := got["gpuworks_infer_ttft_seconds_sum"], got["gpuworks_infer_ttft_seconds_count"]; sum.kind != "counter" || sum.unit != "seconds" || count.kind != "counter" || count.unit != "requests" || sum.quantiles {
		t.Fatalf("ttft sum/count = %+v / %+v", sum, count)
	}
	if ttft := got["gpuworks_proxy_ttft_ms"]; !ttft.quantiles || ttft.source != "gateway" || ttft.unit != "milliseconds" {
		t.Fatalf("gateway ttft = %+v", ttft)
	}
	if running := got["gpuworks_infer_requests_running"]; running.kind != "gauge" || running.quantiles {
		t.Fatalf("running = %+v", running)
	}
	// Every definition is listed.
	for _, definition := range inferencemetrics.Definitions() {
		if _, ok := got[definition.Series()]; !ok {
			t.Fatalf("%s missing", definition.Series())
		}
	}
	for _, definition := range inferencemetrics.GatewayDefinitions() {
		if _, ok := got[definition.Series()]; !ok {
			t.Fatalf("%s missing", definition.Series())
		}
	}
	for _, definition := range platformmetrics.Definitions() {
		if m, ok := got[definition.Series]; !ok || m.source != string(definition.Source) || m.unit != definition.Unit {
			t.Fatalf("%s = %+v, want source %s unit %s", definition.Series, m, definition.Source, definition.Unit)
		}
	}
}

// A platform series carries its labels and what it measures, so a reader of
// the catalog (the explorer, the assistant) knows how to query it.
func TestMetricCatalogDescribesPlatformSeries(t *testing.T) {
	for _, m := range (&Service{}).Metrics() {
		if m.Series != "gpuworks_gpu_utilization" {
			continue
		}
		if m.Source != "gpu" || m.Name != "gpu_utilization" || m.Kind != "gauge" || m.Unit != "percent" || m.Description == "" ||
			!reflect.DeepEqual(m.Labels, []string{"node", "gpu", "gpu_uuid", "vendor"}) {
			t.Fatalf("gpu utilization = %+v", m)
		}
		return
	}
	t.Fatal("gpuworks_gpu_utilization is not in the catalog")
}

// The UI tests list metrics from a recorded response of this endpoint; it is
// the endpoint's exact output, so the explorer's tests see what the server
// actually sends.
func TestMetricVocabularyMatchesTheUIFixture(t *testing.T) {
	var fixture, live any
	encoded, _ := json.MarshalIndent(dto.NewList((&Service{}).Metrics()), "", "  ")
	if *updateFixture {
		if err := os.WriteFile("../../../ui/tests/fixtures/monitor-metrics.json", append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = json.Unmarshal(encoded, &live)
	recorded, err := os.ReadFile("../../../ui/tests/fixtures/monitor-metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recorded, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture, live) {
		t.Fatal("ui/tests/fixtures/monitor-metrics.json is out of date with GET /api/v1/monitor/metrics; regenerate it: go test ./internal/server/monitorapi -run TestMetricVocabularyMatchesTheUIFixture -update")
	}
}
