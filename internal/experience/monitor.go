package experience

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// The monitoring center reads the raw metrics of the last hours. A new
// environment has only minutes of them, so once every series exists
// (the starters run and have been scraped a few times) it writes the day
// before: each series continues backwards from its first live sample along
// the load curve (simulate.Rhythm), so the charts read as one day of the
// same platform.

// backfillSpan and backfillStep are how far back the history goes and how
// dense it is: a sample a minute, against the live 30 s.
const (
	backfillSpan = 24 * time.Hour
	backfillStep = time.Minute
)

// metricShape says how a family's history follows the load: a counter
// grows by its live rate scaled to the load; a gauge takes its live level
// scaled the same way. follow is how strongly: the level moves as
// (follow + load) / (follow + load now), so a small follow moves with the
// load and a large one hardly at all; steady never moves.
type metricShape struct {
	counter bool
	follow  float64
	steady  bool
	round   bool
}

var backfilled = map[string]metricShape{
	"gpuworks_gpu_utilization":         {follow: 0.25},
	"gpuworks_gpu_power_watts":         {follow: 0.6},
	"gpuworks_gpu_temperature_celsius": {follow: 1.5},
	"gpuworks_gpu_memory_used_bytes":   {steady: true},
	"gpuworks_gpu_memory_total_bytes":  {steady: true},

	"gpuworks_node_cpu_busy_percent":         {follow: 0.1},
	"gpuworks_node_mem_used_bytes":           {follow: 3},
	"gpuworks_node_mem_total_bytes":          {steady: true},
	"gpuworks_node_disk_read_bytes_per_sec":  {follow: 0.1},
	"gpuworks_node_disk_write_bytes_per_sec": {follow: 0.1},
	"gpuworks_node_net_rx_bytes_per_sec":     {follow: 0.1},
	"gpuworks_node_net_tx_bytes_per_sec":     {follow: 0.1},
	"gpuworks_node_nic_rx_bytes_per_sec":     {follow: 0.1},
	"gpuworks_node_nic_tx_bytes_per_sec":     {follow: 0.1},

	"gpuworks_infer_requests_running":        {follow: 0.05, round: true},
	"gpuworks_infer_requests_waiting":        {follow: 0.02, round: true},
	"gpuworks_infer_gpu_cache_usage_percent": {follow: 0.4},
	"gpuworks_infer_ttft_seconds":            {follow: 2},
	"gpuworks_infer_tpot_seconds":            {follow: 2},
	"gpuworks_infer_e2e_seconds":             {follow: 2},
	"gpuworks_infer_queue_time_seconds":      {follow: 0.5},
	"gpuworks_infer_prefill_time_seconds":    {follow: 2},
	"gpuworks_infer_decode_time_seconds":     {follow: 2},
	"gpuworks_proxy_requests_inflight":       {follow: 0.05, round: true},
	"gpuworks_proxy_ttft_ms":                 {follow: 2},
	"gpuworks_proxy_request_duration_ms":     {follow: 2},
}

// backfilledCounters are the counter families, by prefix of their name;
// every one grows with the load.
var backfilledCounters = []string{
	"gpuworks_infer_generation_tokens_total", "gpuworks_infer_prompt_tokens_total",
	"gpuworks_infer_requests_success_total", "gpuworks_infer_preemptions_total",
	"gpuworks_infer_kv_queries_total", "gpuworks_infer_kv_hits_total",
	"gpuworks_infer_ttft_seconds_", "gpuworks_infer_tpot_seconds_", "gpuworks_infer_e2e_seconds_",
	"gpuworks_infer_queue_time_seconds_", "gpuworks_infer_prefill_time_seconds_", "gpuworks_infer_decode_time_seconds_",
	"gpuworks_proxy_routed_total", "gpuworks_proxy_requests_total", "gpuworks_proxy_prompt_tokens_total",
	"gpuworks_proxy_completion_tokens_total", "gpuworks_proxy_cached_tokens_total", "gpuworks_proxy_usage_missing_total",
	"gpuworks_proxy_ttft_ms_", "gpuworks_proxy_request_duration_ms_",
}

func init() {
	for _, name := range backfilledCounters {
		if strings.HasSuffix(name, "_") {
			for _, suffix := range []string{"sum", "count"} {
				backfilled[name+suffix] = metricShape{counter: true, follow: 0.05}
			}
			continue
		}
		backfilled[name] = metricShape{counter: true, follow: 0.05}
	}
}

// backfillMetrics writes the day before the environment's first live
// samples, once.
func backfillMetrics(ctx context.Context, app *serverapp.App, marker string, log *slog.Logger) {
	if _, err := os.Stat(marker); err == nil {
		return
	}
	// Three scrapes of every starter's instances give each series a level
	// and a rate to continue from.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	now := time.Now()
	rng := rand.New(rand.NewPCG(uint64(now.Unix()), 13))
	var batch []tsdb.Sample
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		if err := app.SeedMetrics(batch); err != nil {
			log.Warn("metric history not written", "err", err)
			return false
		}
		batch = batch[:0]
		return true
	}
	metrics := make([]string, 0, len(backfilled))
	for metric := range backfilled {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics) // a stable order, whatever the map's
	for _, metric := range metrics {
		shape := backfilled[metric]
		series, err := app.MetricSeries(metric, now.Add(-10*time.Minute).UnixMilli())
		if err != nil {
			log.Warn("metric history skipped", "metric", metric, "err", err)
			continue
		}
		for _, s := range series {
			batch = append(batch, backfillSeries(metric, s, shape, now, rng)...)
			if len(batch) >= 20000 && !flush() {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
	if flush() {
		_ = os.WriteFile(marker, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
}

// backfillSeries is one series' day before its first live point.
func backfillSeries(metric string, s tsdb.Series, shape metricShape, now time.Time, rng *rand.Rand) []tsdb.Sample {
	if len(s.Points) == 0 {
		return nil
	}
	first, last := s.Points[0], s.Points[len(s.Points)-1]
	firstAt := time.UnixMilli(first.TimeMS)
	scale := func(t time.Time) float64 {
		if shape.steady {
			return 1
		}
		return (shape.follow + simulate.Rhythm(t)) / (shape.follow + simulate.Rhythm(now))
	}
	var times []time.Time
	for t := firstAt.Add(-backfillSpan).Truncate(backfillStep); t.Before(firstAt.Add(-backfillStep / 2)); t = t.Add(backfillStep) {
		times = append(times, t)
	}
	out := make([]tsdb.Sample, len(times))
	if shape.counter {
		span := float64(last.TimeMS-first.TimeMS) / 1000
		if span < 30 {
			return nil
		}
		rate := math.Max(0, last.Value-first.Value) / span
		// Grow up to the first live point; a counter younger than the day
		// (the gateway's, kept since the console started) grows from zero
		// instead, and its first live point reads as the restart it is.
		sums := make([]float64, len(times))
		total := 0.0
		for i, t := range times {
			total += rate * scale(t) * backfillStep.Seconds() * (0.9 + rng.Float64()*0.2)
			sums[i] = total
		}
		base := 0.0
		if total <= first.Value {
			base = first.Value - total
		}
		for i, t := range times {
			out[i] = tsdb.Sample{Metric: metric, Labels: s.Labels, Value: base + sums[i], TimeMS: t.UnixMilli()}
		}
		return out
	}
	level := 0.0
	for _, p := range s.Points {
		level += p.Value
	}
	level /= float64(len(s.Points))
	for i, t := range times {
		v := level * scale(t)
		if !shape.steady {
			v *= 1 + rng.NormFloat64()*0.05
		}
		if shape.round {
			v = math.Round(v)
		}
		out[i] = tsdb.Sample{Metric: metric, Labels: s.Labels, Value: math.Max(0, v), TimeMS: t.UnixMilli()}
	}
	return out
}
