package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// registerMetrics exposes a small, deterministic Prometheus text endpoint so
// L2 can assert the agent collector's histogram/counter/gauge parsing (plan
// §4.1/§4.2) end to end without a real vLLM/SGLang process.
//
// Every exposition line carries model_name, matching real vLLM/SGLang (every
// metric in those engines' Prometheus registries is labeled by model). Every
// counter/histogram is stateful across scrapes: each /metrics request adds a
// fixed increment (MetricsObservationsPerScrape identical-value
// observations, and PromptTokens/CompletionTokens worth of new tokens), so
// two successive scrapes show nonzero rate() exactly like a real engine under
// steady load. Gauges (num_requests_running/waiting, kv_cache_usage_perc)
// stay fixed at their configured value, since a gauge does not "advance".
func registerMetrics(mux *http.ServeMux, opts Options, stats *engineStats) {
	dialect := opts.Dialect
	if dialect == "" {
		dialect = "vllm"
	}
	modelName := opts.Model
	observationsPerScrape := int64(opts.MetricsObservationsPerScrape)
	if observationsPerScrape <= 0 {
		observationsPerScrape = 1
	}
	var scrapes atomic.Int64
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		n := scrapes.Add(1)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(renderMetrics(dialect, modelName, opts, stats, n, observationsPerScrape)))
		if opts.ExtraMetrics != nil {
			_, _ = w.Write([]byte(opts.ExtraMetrics()))
		}
	})
}

// Real vLLM v0.9.x bucket boundaries (vllm/engine/metrics.py), reused
// verbatim rather than an invented round-number list, so bucket counts
// exercise the same shape a real engine's exposition has.
var (
	ttftBucketsSeconds = []float64{
		0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5,
		0.75, 1.0, 2.5, 5.0, 7.5, 10.0, 20.0, 40.0, 80.0, 160.0, 640.0, 2560.0,
	}
	tpotBucketsSeconds = []float64{
		0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75,
		1.0, 2.5, 5.0, 7.5, 10.0, 20.0, 40.0, 80.0,
	}
	// requestLatencyBucketsSeconds backs e2e/queue/prefill/decode in real
	// vLLM: one shared bucket list for every request-phase histogram.
	requestLatencyBucketsSeconds = []float64{
		0.3, 0.5, 0.8, 1.0, 1.5, 2.0, 2.5, 5.0, 10.0, 15.0, 20.0, 30.0,
		40.0, 50.0, 60.0, 120.0, 240.0, 480.0, 960.0, 1920.0, 7680.0,
	}
)

// renderMetrics builds the full exposition body for one dialect and scrape.
// SGLang only emits families verified against sgl-project/sglang@main
// python/sglang/srt/observability/metrics_collector.py (see the MetricMap
// comment in internal/server/deploy/sglang.go for the exact citation and
// which names were checked): prefill/decode time and a bare request-success
// counter have no verified SGLang exposition name, so the sglang dialect
// omits them rather than inventing one.
func renderMetrics(dialect, modelName string, opts Options, stats *engineStats, scrapes, observationsPerScrape int64) string {
	var b strings.Builder
	totalObservations := scrapes * observationsPerScrape

	writeHistogram(&b, familyName(dialect, "ttft"), modelName, ttftBucketsSeconds, opts.MetricsTTFTSeconds, totalObservations)
	writeHistogram(&b, familyName(dialect, "tpot"), modelName, tpotBucketsSeconds, opts.MetricsTPOTSeconds, totalObservations)
	writeHistogram(&b, familyName(dialect, "e2e"), modelName, requestLatencyBucketsSeconds, opts.MetricsE2ESeconds, totalObservations)
	writeHistogram(&b, familyName(dialect, "queue"), modelName, requestLatencyBucketsSeconds, opts.MetricsQueueSeconds, totalObservations)
	if dialect != "sglang" {
		writeHistogram(&b, familyName(dialect, "prefill"), modelName, requestLatencyBucketsSeconds, opts.MetricsPrefillSeconds, totalObservations)
		writeHistogram(&b, familyName(dialect, "decode"), modelName, requestLatencyBucketsSeconds, opts.MetricsDecodeSeconds, totalObservations)
	}

	// Gauges: fixed at the configured value on every scrape, plus whatever
	// real /v1/chat/completions traffic is live right now — a harness that
	// never drives real requests sees the exact configured numbers (live
	// deltas are 0), one that does sees the gauges move with it.
	var liveRunning, liveWaiting, livePromptTokens, liveGenerationTokens int64
	if stats != nil {
		if !opts.StaticGauges {
			liveRunning = stats.running.Load()
			liveWaiting = stats.waiting.Load()
		}
		livePromptTokens = stats.promptTokens.Load()
		liveGenerationTokens = stats.generationTokens.Load()
	}
	writeGauge(&b, gaugeName(dialect, "running"), modelName, float64(int64(opts.MetricsRequestsRunning)+liveRunning))
	writeGauge(&b, gaugeName(dialect, "waiting"), modelName, float64(int64(opts.MetricsRequestsWaiting)+liveWaiting))
	writeGauge(&b, gaugeName(dialect, "cache_usage"), modelName, opts.MetricsKVCacheUsagePercent)

	// Counters: real engines only ever grow these, so every scrape adds
	// PromptTokens/CompletionTokens worth of new tokens (the same per-request
	// token counts already used for the OpenAI-shaped responses), plus
	// whatever real requests have actually consumed so far.
	writeCounter(&b, tokensCounterName(dialect, "prompt"), modelName, float64(scrapes*int64(opts.PromptTokens)+livePromptTokens))
	writeCounter(&b, tokensCounterName(dialect, "generation"), modelName, float64(scrapes*int64(opts.CompletionTokens)+liveGenerationTokens))
	writeCounter(&b, preemptionsCounterName(dialect), modelName, float64(scrapes*int64(opts.MetricsPreemptionsPerScrape)))

	if dialect != "sglang" {
		// Real vLLM never exposes a bare vllm:request_success_total: the
		// counter is always split by finished_reason.
		fmt.Fprintln(&b, "# HELP vllm:request_success_total Count of successfully processed requests.")
		fmt.Fprintln(&b, "# TYPE vllm:request_success_total counter")
		writeLabeledCounter(&b, "vllm:request_success_total", modelName, "finished_reason", "stop", float64(scrapes*int64(opts.MetricsRequestSuccessStopPerScrape)))
		writeLabeledCounter(&b, "vllm:request_success_total", modelName, "finished_reason", "length", float64(scrapes*int64(opts.MetricsRequestSuccessLengthPerScrape)))
		writeLabeledCounter(&b, "vllm:request_success_total", modelName, "finished_reason", "abort", float64(scrapes*int64(opts.MetricsRequestSuccessAbortPerScrape)))
	}

	return b.String()
}

func familyName(dialect, kind string) string {
	if dialect == "sglang" {
		switch kind {
		case "ttft":
			return "sglang:time_to_first_token_seconds"
		case "tpot":
			return "sglang:inter_token_latency_seconds"
		case "e2e":
			return "sglang:e2e_request_latency_seconds"
		case "queue":
			return "sglang:queue_time_seconds"
		default:
			return ""
		}
	}
	switch kind {
	case "ttft":
		return "vllm:time_to_first_token_seconds"
	case "tpot":
		// vLLM >= 0.10 renamed this family to inter_token_latency_seconds
		// (confirmed on a real vLLM 0.25.1 /metrics); the fake engine exposes
		// the current name so fixtures track what a real engine emits today.
		return "vllm:inter_token_latency_seconds"
	case "e2e":
		return "vllm:e2e_request_latency_seconds"
	case "queue":
		return "vllm:request_queue_time_seconds"
	case "prefill":
		return "vllm:request_prefill_time_seconds"
	case "decode":
		return "vllm:request_decode_time_seconds"
	default:
		return ""
	}
}

// gaugeName returns each dialect's real exposition name for the collector's
// default-mapped running/waiting/cache-usage gauges (deploy/vllm.go and
// deploy/sglang.go's own MetricMaps use different names per engine).
func gaugeName(dialect, kind string) string {
	if dialect == "sglang" {
		switch kind {
		case "running":
			return "sglang:num_running_reqs"
		case "waiting":
			return "sglang:num_queue_reqs"
		case "cache_usage":
			return "sglang:token_usage"
		}
	}
	switch kind {
	case "running":
		return "vllm:num_requests_running"
	case "waiting":
		return "vllm:num_requests_waiting"
	case "cache_usage":
		return "vllm:kv_cache_usage_perc"
	}
	return ""
}

func tokensCounterName(dialect, kind string) string {
	prefix := "vllm"
	if dialect == "sglang" {
		prefix = "sglang"
	}
	if kind == "prompt" {
		return prefix + ":prompt_tokens_total"
	}
	return prefix + ":generation_tokens_total"
}

func preemptionsCounterName(dialect string) string {
	if dialect == "sglang" {
		return "sglang:num_retracted_requests_total"
	}
	return "vllm:num_preemptions_total"
}

// writeHistogram renders one Prometheus histogram family for totalCount
// identical-value observations: every bucket whose "le" bound is at or above
// value cumulates totalCount (including "+Inf"), every bucket below it
// cumulates 0 — the same step-function shape a real engine has when every
// request so far took exactly `value` seconds.
func writeHistogram(b *strings.Builder, name, modelName string, buckets []float64, value float64, totalCount int64) {
	if name == "" {
		return
	}
	fmt.Fprintf(b, "# HELP %s fake-engine deterministic histogram (see FAKE_ENGINE_METRICS_* env vars).\n", name)
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)
	for _, le := range buckets {
		count := int64(0)
		if value <= le {
			count = totalCount
		}
		fmt.Fprintf(b, "%s_bucket{model_name=%q,le=%q} %d\n", name, modelName, formatFloat(le), count)
	}
	fmt.Fprintf(b, "%s_bucket{model_name=%q,le=\"+Inf\"} %d\n", name, modelName, totalCount)
	fmt.Fprintf(b, "%s_sum{model_name=%q} %s\n", name, modelName, formatFloat(value*float64(totalCount)))
	fmt.Fprintf(b, "%s_count{model_name=%q} %d\n", name, modelName, totalCount)
}

func writeGauge(b *strings.Builder, name, modelName string, value float64) {
	if name == "" {
		return
	}
	fmt.Fprintf(b, "# HELP %s fake-engine deterministic gauge (see FAKE_ENGINE_METRICS_* env vars).\n", name)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s{model_name=%q} %s\n", name, modelName, formatFloat(value))
}

func writeCounter(b *strings.Builder, name, modelName string, value float64) {
	if name == "" {
		return
	}
	fmt.Fprintf(b, "# HELP %s fake-engine deterministic counter (see FAKE_ENGINE_METRICS_* env vars).\n", name)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s{model_name=%q} %s\n", name, modelName, formatFloat(value))
}

func writeLabeledCounter(b *strings.Builder, name, modelName, extraKey, extraValue string, value float64) {
	fmt.Fprintf(b, "%s{model_name=%q,%s=%q} %s\n", name, modelName, extraKey, extraValue, formatFloat(value))
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
