package experience

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
)

// request is what one answered request adds to its engine's statistics.
type request struct {
	prompt, cached, generated int
	queue, ttft, tpot, e2e    time.Duration
}

// Bucket bounds of the latency histograms, as vLLM declares them.
var (
	ttftBuckets = []float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 20, 40, 80}
	tpotBuckets = []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 2.5, 5}
	e2eBuckets  = []float64{0.3, 0.5, 0.8, 1, 1.5, 2, 2.5, 5, 10, 15, 20, 30, 40, 50, 60, 120, 240}
)

// histogram is one Prometheus histogram: cumulative counts per bound.
type histogram struct {
	bounds []float64
	counts []float64 // per bound, then +Inf
	sum    float64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]float64, len(bounds)+1)}
}

func (h *histogram) observe(v, n float64) {
	h.sum += v * n
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i] += n
		}
	}
	h.counts[len(h.bounds)] += n
}

func (h *histogram) write(b *strings.Builder, name, labels string) {
	fmt.Fprintf(b, "# TYPE %s histogram\n", name)
	for i, bound := range h.bounds {
		fmt.Fprintf(b, "%s_bucket{%sle=\"%g\"} %g\n", name, labels, bound, h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{%sle=\"+Inf\"} %g\n", name, labels, h.counts[len(h.bounds)])
	fmt.Fprintf(b, "%s_sum{%s} %g\n%s_count{%s} %g\n", name, strings.TrimSuffix(labels, ","), h.sum, name, strings.TrimSuffix(labels, ","), h.counts[len(h.bounds)])
}

// engineStats are one engine port's counters. They start as an engine that
// has served for a day would read, so the history the environment writes
// before it (monitor.go) leads up to them.
type engineStats struct {
	mu                                           sync.Mutex
	prompt, generation, queries, hits, succeeded float64
	ttft, tpot, e2e, queue, prefill, decode      *histogram
}

// uptimeRequests is how many requests a port's counters start with.
const uptimeRequests = 2_000_000

func newEngineStats() *engineStats {
	s := &engineStats{
		ttft: newHistogram(ttftBuckets), tpot: newHistogram(tpotBuckets), e2e: newHistogram(e2eBuckets),
		queue: newHistogram(e2eBuckets), prefill: newHistogram(e2eBuckets), decode: newHistogram(e2eBuckets),
	}
	s.observe(request{prompt: 2400, cached: 1300, generated: 320, queue: 10 * time.Millisecond,
		ttft: 380 * time.Millisecond, tpot: 20 * time.Millisecond, e2e: 6800 * time.Millisecond}, uptimeRequests)
	return s
}

// observe counts r, n times.
func (s *engineStats) observe(r request, n ...float64) {
	times := 1.0
	if len(n) > 0 {
		times = n[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompt += float64(r.prompt) * times
	s.generation += float64(r.generated) * times
	s.queries += float64(r.prompt) * times
	s.hits += float64(r.cached) * times
	s.succeeded += times
	s.ttft.observe(r.ttft.Seconds(), times)
	s.tpot.observe(r.tpot.Seconds(), float64(max(r.generated-1, 1))*times)
	s.e2e.observe(r.e2e.Seconds(), times)
	s.queue.observe(r.queue.Seconds(), times)
	s.prefill.observe((r.ttft - r.queue).Seconds(), times)
	s.decode.observe((r.e2e - r.ttft).Seconds(), times)
}

// metrics is the engine's /metrics page for each model on the port, in both
// vLLM's and SGLang's names: the platform reads the dialect of the engine
// the deployment runs and ignores the other.
func (p *enginePort) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	running := float64(p.running.Load())
	waiting := math.Max(0, running-engineBatch)
	load := simulate.Rhythm(time.Now())
	cacheUsage := math.Min(0.95, 0.15+0.45*load+0.012*running)
	s := p.stats
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, model := range p.models() {
		labels := fmt.Sprintf("model_name=%q,", model)
		plain := fmt.Sprintf("{model_name=%q}", model)
		gauge := func(name string, v float64) { fmt.Fprintf(&b, "# TYPE %s gauge\n%s%s %g\n", name, name, plain, v) }
		counter := func(name string, v float64) { fmt.Fprintf(&b, "# TYPE %s counter\n%s%s %g\n", name, name, plain, v) }

		gauge("vllm:num_requests_running", running)
		gauge("vllm:num_requests_waiting", waiting)
		gauge("vllm:kv_cache_usage_perc", cacheUsage)
		counter("vllm:prompt_tokens_total", s.prompt)
		counter("vllm:generation_tokens_total", s.generation)
		counter("vllm:prefix_cache_queries_total", s.queries)
		counter("vllm:prefix_cache_hits_total", s.hits)
		counter("vllm:num_preemptions_total", 0)
		fmt.Fprintf(&b, "# TYPE vllm:request_success_total counter\nvllm:request_success_total{finished_reason=\"stop\",model_name=%q} %g\n", model, s.succeeded)
		s.ttft.write(&b, "vllm:time_to_first_token_seconds", labels)
		s.tpot.write(&b, "vllm:inter_token_latency_seconds", labels)
		s.e2e.write(&b, "vllm:e2e_request_latency_seconds", labels)
		s.queue.write(&b, "vllm:request_queue_time_seconds", labels)
		s.prefill.write(&b, "vllm:request_prefill_time_seconds", labels)
		s.decode.write(&b, "vllm:request_decode_time_seconds", labels)

		gauge("sglang:num_running_reqs", running)
		gauge("sglang:num_queue_reqs", waiting)
		gauge("sglang:token_usage", cacheUsage)
		gauge("sglang:cache_hit_rate", s.hits/math.Max(s.queries, 1))
		counter("sglang:prompt_tokens_total", s.prompt)
		counter("sglang:generation_tokens_total", s.generation)
		counter("sglang:num_retracted_requests_total", 0)
		s.ttft.write(&b, "sglang:time_to_first_token_seconds", labels)
		s.tpot.write(&b, "sglang:inter_token_latency_seconds", labels)
		s.e2e.write(&b, "sglang:e2e_request_latency_seconds", labels)
		s.queue.write(&b, "sglang:queue_time_seconds", labels)
	}
	_, _ = io.WriteString(w, b.String())
}
