package handler

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// scrape fetches /metrics and returns it split into lines, failing the test
// on any transport/status problem so callers only deal with content.
func scrape(t *testing.T, server *httptest.Server) []string {
	t.Helper()
	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func scrapeMetrics(t *testing.T, opts Options) []string {
	t.Helper()
	server := httptest.NewServer(New(opts))
	defer server.Close()
	return scrape(t, server)
}

// floatsClose tolerates ordinary float64 rounding (e.g. 0.05*3 landing a
// couple ULPs away from 0.15) without accepting a genuinely wrong value.
func floatsClose(a, b float64) bool {
	const epsilon = 1e-9
	d := a - b
	return d > -epsilon && d < epsilon
}

func lineFor(lines []string, prefix string) (string, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l, true
		}
	}
	return "", false
}

// valueOf returns the trailing numeric field of an exposition line (the part
// after the last space), which is where the value always lives regardless of
// how many labels precede it.
func valueOf(t *testing.T, line string) float64 {
	t.Helper()
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		t.Fatalf("no value field in line %q", line)
	}
	v, err := strconv.ParseFloat(line[sp+1:], 64)
	if err != nil {
		t.Fatalf("value field in line %q: %v", line, err)
	}
	return v
}

// The vLLM dialect (the default) exposes every family the plan's collector
// MetricMap maps: TTFT/TPOT/E2E/queue/prefill/decode histograms, the
// num_requests_running/waiting + kv_cache_usage_perc gauges, prompt/
// generation token counters, num_preemptions_total, and request_success_total
// split by finished_reason — each as a real, model_name-labeled line an agent
// collector can actually parse.
func TestMetricsVLLMDialectExposesFullFamilySet(t *testing.T) {
	lines := scrapeMetrics(t, Options{
		Model: "fake-model", MetricsTTFTSeconds: 0.05,
		MetricsPreemptionsPerScrape: 3, MetricsRequestSuccessStopPerScrape: 5,
		MetricsRequestsRunning: 2, MetricsRequestsWaiting: 1, MetricsKVCacheUsagePercent: 0.4,
	})
	for _, name := range []string{
		"vllm:time_to_first_token_seconds", "vllm:inter_token_latency_seconds", "vllm:e2e_request_latency_seconds",
		"vllm:request_queue_time_seconds", "vllm:request_prefill_time_seconds", "vllm:request_decode_time_seconds",
	} {
		if _, ok := lineFor(lines, name+"_count{"); !ok {
			t.Fatalf("missing %s_count in:\n%s", name, strings.Join(lines, "\n"))
		}
		if _, ok := lineFor(lines, name+"_sum{"); !ok {
			t.Fatalf("missing %s_sum in:\n%s", name, strings.Join(lines, "\n"))
		}
	}
	for _, name := range []string{"vllm:num_requests_running", "vllm:num_requests_waiting", "vllm:kv_cache_usage_perc", "vllm:prompt_tokens_total", "vllm:generation_tokens_total", "vllm:num_preemptions_total"} {
		if _, ok := lineFor(lines, name+"{"); !ok {
			t.Fatalf("missing %s in:\n%s", name, strings.Join(lines, "\n"))
		}
	}
	if got, ok := lineFor(lines, `vllm:num_requests_running{model_name="fake-model"}`); !ok || valueOf(t, got) != 2 {
		t.Fatalf("num_requests_running = %q, ok=%v", got, ok)
	}
	if got, ok := lineFor(lines, `vllm:kv_cache_usage_perc{model_name="fake-model"}`); !ok || valueOf(t, got) != 0.4 {
		t.Fatalf("kv_cache_usage_perc = %q, ok=%v", got, ok)
	}
	if got, ok := lineFor(lines, `vllm:num_preemptions_total{model_name="fake-model"}`); !ok || valueOf(t, got) != 3 {
		t.Fatalf("num_preemptions_total (scrape 1) = %q, ok=%v, want 3", got, ok)
	}
	if got, ok := lineFor(lines, `vllm:request_success_total{model_name="fake-model",finished_reason="stop"}`); !ok || valueOf(t, got) != 5 {
		t.Fatalf("request_success_total{finished_reason=stop} = %q, ok=%v, want 5", got, ok)
	}
}

// Every exposition line (bucket, sum, count, gauge, counter) carries
// model_name, matching real vLLM/SGLang where every metric in the registry is
// labeled by model.
func TestMetricsEveryLineCarriesModelName(t *testing.T) {
	lines := scrapeMetrics(t, Options{Model: "qwen-fake"})
	checked := 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		checked++
		if !strings.Contains(l, `model_name="qwen-fake"`) {
			t.Fatalf("line lacks model_name label: %q", l)
		}
	}
	if checked == 0 {
		t.Fatal("no data lines were scraped")
	}
}

// The TTFT histogram is a step function of the single fixed value every
// observation takes: a bucket below the value cumulates 0, every bucket at or
// above it (including +Inf) cumulates the total observation count, and
// sum/count reflect that same total — using vLLM's own bucket boundaries.
func TestMetricsHistogramBucketsAreExactStepFunctionOfFixedValue(t *testing.T) {
	lines := scrapeMetrics(t, Options{Model: "fake-model", MetricsTTFTSeconds: 0.05, MetricsObservationsPerScrape: 3})
	name := "vllm:time_to_first_token_seconds"
	// 0.05 sits strictly between the real boundaries 0.04 and 0.06.
	if got, ok := lineFor(lines, name+`_bucket{model_name="fake-model",le="0.04"}`); !ok || valueOf(t, got) != 0 {
		t.Fatalf("bucket below value = %q, ok=%v, want count 0", got, ok)
	}
	if got, ok := lineFor(lines, name+`_bucket{model_name="fake-model",le="0.06"}`); !ok || valueOf(t, got) != 3 {
		t.Fatalf("bucket at/above value = %q, ok=%v, want count 3", got, ok)
	}
	if got, ok := lineFor(lines, name+`_bucket{model_name="fake-model",le="+Inf"}`); !ok || valueOf(t, got) != 3 {
		t.Fatalf("+Inf bucket = %q, ok=%v, want count 3", got, ok)
	}
	if got, ok := lineFor(lines, name+`_sum{`); !ok || !floatsClose(valueOf(t, got), 0.15) {
		t.Fatalf("sum = %q, ok=%v, want ~0.15 (0.05*3)", got, ok)
	}
	if got, ok := lineFor(lines, name+`_count{`); !ok || valueOf(t, got) != 3 {
		t.Fatalf("count = %q, ok=%v, want 3", got, ok)
	}
}

// The histogram bucket lists are vLLM's real ones (vllm/engine/metrics.py),
// not an invented round-number list — this is what "~20+ buckets" means in
// practice for TTFT and the shared request-latency families.
func TestMetricsBucketListsMatchRealVLLMBoundaries(t *testing.T) {
	if len(ttftBucketsSeconds) != 22 {
		t.Fatalf("ttft buckets = %d, want 22 (real vLLM v0.9.x list)", len(ttftBucketsSeconds))
	}
	if len(tpotBucketsSeconds) != 19 {
		t.Fatalf("tpot buckets = %d, want 19 (real vLLM v0.9.x list)", len(tpotBucketsSeconds))
	}
	if len(requestLatencyBucketsSeconds) != 21 {
		t.Fatalf("request-latency buckets = %d, want 21 (real vLLM v0.9.x list, shared by e2e/queue/prefill/decode)", len(requestLatencyBucketsSeconds))
	}
}

// Every counter and histogram is stateful across scrapes: a second scrape
// shows a nonzero rate(), exactly like a real engine under steady load.
// Gauges are the opposite — they stay fixed at their configured value.
func TestMetricsCountersAndHistogramsAdvancePerScrapeButGaugesStayFixed(t *testing.T) {
	server := httptest.NewServer(New(Options{
		Model: "fake-model", MetricsTTFTSeconds: 0.05, PromptTokens: 16, CompletionTokens: 8,
		MetricsPreemptionsPerScrape: 2, MetricsRequestSuccessStopPerScrape: 4,
		MetricsRequestsRunning: 1, MetricsKVCacheUsagePercent: 0.3,
	}))
	defer server.Close()

	first := scrape(t, server)
	second := scrape(t, server)

	firstCount, _ := lineFor(first, `vllm:time_to_first_token_seconds_count{`)
	secondCount, _ := lineFor(second, `vllm:time_to_first_token_seconds_count{`)
	if valueOf(t, firstCount) != 1 || valueOf(t, secondCount) != 2 {
		t.Fatalf("ttft _count across scrapes = %v -> %v, want 1 -> 2", valueOf(t, firstCount), valueOf(t, secondCount))
	}

	firstPreempt, _ := lineFor(first, `vllm:num_preemptions_total{`)
	secondPreempt, _ := lineFor(second, `vllm:num_preemptions_total{`)
	if valueOf(t, firstPreempt) != 2 || valueOf(t, secondPreempt) != 4 {
		t.Fatalf("preemptions across scrapes = %v -> %v, want 2 -> 4", valueOf(t, firstPreempt), valueOf(t, secondPreempt))
	}

	firstPrompt, _ := lineFor(first, `vllm:prompt_tokens_total{`)
	secondPrompt, _ := lineFor(second, `vllm:prompt_tokens_total{`)
	if valueOf(t, firstPrompt) != 16 || valueOf(t, secondPrompt) != 32 {
		t.Fatalf("prompt_tokens_total across scrapes = %v -> %v, want 16 -> 32", valueOf(t, firstPrompt), valueOf(t, secondPrompt))
	}

	firstSuccess, _ := lineFor(first, `vllm:request_success_total{model_name="fake-model",finished_reason="stop"}`)
	secondSuccess, _ := lineFor(second, `vllm:request_success_total{model_name="fake-model",finished_reason="stop"}`)
	if valueOf(t, firstSuccess) != 4 || valueOf(t, secondSuccess) != 8 {
		t.Fatalf("request_success_total{stop} across scrapes = %v -> %v, want 4 -> 8", valueOf(t, firstSuccess), valueOf(t, secondSuccess))
	}

	firstRunning, _ := lineFor(first, `vllm:num_requests_running{`)
	secondRunning, _ := lineFor(second, `vllm:num_requests_running{`)
	if valueOf(t, firstRunning) != 1 || valueOf(t, secondRunning) != 1 {
		t.Fatalf("num_requests_running (gauge) across scrapes = %v -> %v, want 1 -> 1 (unchanged)", valueOf(t, firstRunning), valueOf(t, secondRunning))
	}
	firstCache, _ := lineFor(first, `vllm:kv_cache_usage_perc{`)
	secondCache, _ := lineFor(second, `vllm:kv_cache_usage_perc{`)
	if valueOf(t, firstCache) != 0.3 || valueOf(t, secondCache) != 0.3 {
		t.Fatalf("kv_cache_usage_perc (gauge) across scrapes = %v -> %v, want 0.3 -> 0.3 (unchanged)", valueOf(t, firstCache), valueOf(t, secondCache))
	}
}

// vllm:request_success_total is always split by finished_reason in real
// vLLM (there is no bare/unlabeled row); each reason is independently
// configurable and additive, not a single fixed total.
func TestMetricsRequestSuccessSplitByFinishedReason(t *testing.T) {
	lines := scrapeMetrics(t, Options{
		Model:                                "fake-model",
		MetricsRequestSuccessStopPerScrape:   10,
		MetricsRequestSuccessLengthPerScrape: 2,
		MetricsRequestSuccessAbortPerScrape:  1,
	})
	for reason, want := range map[string]float64{"stop": 10, "length": 2, "abort": 1} {
		got, ok := lineFor(lines, `vllm:request_success_total{model_name="fake-model",finished_reason="`+reason+`"}`)
		if !ok || valueOf(t, got) != want {
			t.Fatalf("finished_reason=%s = %q, ok=%v, want %v", reason, got, ok, want)
		}
	}
}

// The SGLang dialect only exposes families verified against the real sglang
// source (see internal/server/deploy/sglang.go's MetricMap comment): no
// prefill/decode time and no request-success counter, since neither has a
// verified exposition name; the already-mapped gauges/counters use SGLang's
// own real names, not vLLM's.
func TestMetricsSGLangDialectOmitsUnverifiedFamilies(t *testing.T) {
	lines := scrapeMetrics(t, Options{Model: "fake-model", Dialect: "sglang", MetricsPreemptionsPerScrape: 2, MetricsRequestsRunning: 3})
	for _, name := range []string{"sglang:time_to_first_token_seconds", "sglang:inter_token_latency_seconds", "sglang:e2e_request_latency_seconds", "sglang:queue_time_seconds"} {
		if _, ok := lineFor(lines, name+"_count{"); !ok {
			t.Fatalf("missing %s_count in:\n%s", name, strings.Join(lines, "\n"))
		}
	}
	for _, name := range []string{"sglang:num_running_reqs", "sglang:num_queue_reqs", "sglang:token_usage", "sglang:prompt_tokens_total", "sglang:generation_tokens_total"} {
		if _, ok := lineFor(lines, name+"{"); !ok {
			t.Fatalf("missing %s in:\n%s", name, strings.Join(lines, "\n"))
		}
	}
	if got, ok := lineFor(lines, `sglang:num_retracted_requests_total{model_name="fake-model"}`); !ok || valueOf(t, got) != 2 {
		t.Fatalf("retracted counter = %q, ok=%v, want 2", got, ok)
	}
	if got, ok := lineFor(lines, `sglang:num_running_reqs{model_name="fake-model"}`); !ok || valueOf(t, got) != 3 {
		t.Fatalf("num_running_reqs = %q, ok=%v, want 3", got, ok)
	}
	for _, forbidden := range []string{"vllm:", "request_prefill_time", "request_decode_time", "request_success_total"} {
		for _, l := range lines {
			if strings.Contains(l, forbidden) {
				t.Fatalf("sglang dialect must not expose unverified/vllm-prefixed line: %q", l)
			}
		}
	}
}

func TestMetricsRejectsNonGET(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()
	resp, err := http.Post(server.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
