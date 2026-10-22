package aihelper

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSearchMetricsFindsSeriesByTheirWords(t *testing.T) {
	f := newFakeAPI()
	env := apiEnv{f}
	// Series matching more words come first; the gpuworks_ prefix matches
	// nothing, so "gpu" does not pull in the node and network series.
	out := mustCall(t, env, "search_metrics", `{"query":"gpu memory"}`)
	want := "gpuworks_gpu_memory_used_bytes — GPU memory in use [gpu, gauge, bytes; labels: node, gpu, gpu_uuid]\n" +
		"gpuworks_gpu_temperature_celsius — GPU die temperature [gpu, gauge, celsius; labels: node, gpu]"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	// The source and the description match too; a histogram says how its
	// quantiles are labelled.
	for q, want := range map[string]string{
		"network":     "gpuworks_node_rdma_rx_bytes — RDMA bytes received [network, counter, bytes; labels: node, hca]",
		"first token": "vllm:time_to_first_token_seconds — Time to first token [engine, histogram, seconds, quantile label p50/p95/p99]",
	} {
		if out := mustCall(t, env, "search_metrics", `{"query":"`+q+`"}`); out != want {
			t.Fatalf("%q: %q", q, out)
		}
	}
	if out := mustCall(t, env, "search_metrics", `{"query":"gpuworks"}`); out != "no stored series matches; try other words (gpu, memory, utilization, node, network, rdma, ttft, tokens, kv)" {
		t.Fatalf("no match: %q", out)
	}
	assertIssued(t, f, "/api/v1/monitor/metrics", "/api/v1/monitor/metrics", "/api/v1/monitor/metrics", "/api/v1/monitor/metrics")
}

func TestSearchMetricsCapsItsAnswer(t *testing.T) {
	f := newFakeAPI()
	f.handle("/api/v1/monitor/metrics", func(w http.ResponseWriter, _ *http.Request) {
		items := make([]any, 35)
		for i := range items {
			items[i] = map[string]any{"series": fmt.Sprintf("gpuworks_node_s%d", i), "name": "s", "source": "node", "kind": "gauge", "unit": "bytes", "quantiles": false, "labels": []string{"node"}, "description": "a node series"}
		}
		writeFake(w, 200, map[string]any{"items": items, "total": len(items)})
	})
	ls := lines(mustCall(t, apiEnv{f}, "search_metrics", `{"query":"node"}`))
	if len(ls) != 31 || ls[0] != "gpuworks_node_s0 — a node series [node, gauge, bytes; labels: node]" || ls[30] != "(5 more match; use more specific words)" {
		t.Fatalf("%d lines: first %q last %q", len(ls), ls[0], ls[len(ls)-1])
	}
}

func TestSearchMetricsReportsACatalogError(t *testing.T) {
	f := newFakeAPI()
	f.handle("/api/v1/monitor/metrics", func(w http.ResponseWriter, _ *http.Request) {
		writeProductErr(w, 403, "FORBIDDEN", "forbidden", nil)
	})
	if got := callErr(t, apiEnv{f}, "search_metrics", `{"query":"gpu"}`); got != "metric catalog: HTTP 403" {
		t.Fatalf("error = %q", got)
	}
}

// The overview reads a fixed, bounded handful of pages whatever the fleet's
// size.
func TestClusterOverviewIssuesBoundedReads(t *testing.T) {
	atFakeNow(t)
	f := newFakeAPI()
	out := mustCall(t, apiEnv{f}, "cluster_overview", `{}`)
	assertIssued(t, f,
		"/api/v1/dashboard/summary",
		"/api/v1/gpus?page=1&page_size=1",
		"/api/v1/nodes?page=1&page_size=30&state=offline%2Cconnecting",
		"/api/v1/deployments?page=1&page_size=30&state=failed%2Cpending",
		"/api/v1/deployments?page=1&page_size=20",
	)
	want := `{"deployments_needing_attention":{"items":[{"id":13,"model_name":"Llama-3-70B","name":"llama","node_names":["gpu-b"],"replicas":2,"runtime_state":"failed"}],"total":1},` +
		`"gpus":{"allocated":1,"healthy":2,"total":3,"unhealthy":1},` +
		`"newest_deployments":{"items":[{"id":12,"model_name":"Qwen3-8B","name":"qwen","node_names":["gpu-a"],"replicas":1,"runtime_state":"running"},{"id":13,"model_name":"Llama-3-70B","name":"llama","node_names":["gpu-b"],"replicas":2,"runtime_state":"failed"}],"total":2},` +
		`"nodes_needing_attention":{"items":[{"gpus":["A100 x1"],"last_seen_at":"2026-10-11T10:30:00Z (1.5h ago)","name":"gpu-b","scheduling":"schedulable","state":"offline"}],"total":1},` +
		`"now":"2026-10-11T12:00:00Z",` +
		`"summary":{"by_vendor":[{"gpus":3,"vendor":"nvidia"}],"deployments":{"failed":1,"running":1,"total":2},"nodes":{"gpus":3,"offline":1,"online":1,"total":2}}}`
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

// A read that fails leaves its part unavailable; the rest still answers.
func TestClusterOverviewDegradesAFailedRead(t *testing.T) {
	atFakeNow(t)
	f := newFakeAPI()
	f.handle("/api/v1/dashboard/summary", func(w http.ResponseWriter, _ *http.Request) {
		writeProductErr(w, 500, "INTERNAL", "database is locked", nil)
	})
	f.remove("/api/v1/gpus")
	out := mustCall(t, apiEnv{f}, "cluster_overview", `{}`)
	for _, want := range []string{
		`"summary":"unavailable (HTTP 500 \u003cnil\u003e)"`,
		`"gpus":"unavailable (HTTP 0 not a readable API route: /api/v1/gpus)"`,
		`"nodes_needing_attention":{"items":[{"gpus":["A100 x1"]`,
		`"newest_deployments":{"items":[{"id":12,`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("overview lacks %s:\n%s", want, out)
		}
	}
	assertIssued(t, f,
		"/api/v1/dashboard/summary",
		"/api/v1/nodes?page=1&page_size=30&state=offline%2Cconnecting",
		"/api/v1/deployments?page=1&page_size=30&state=failed%2Cpending",
		"/api/v1/deployments?page=1&page_size=20",
	)
}
