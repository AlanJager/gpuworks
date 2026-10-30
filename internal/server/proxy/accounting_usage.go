package proxy

import (
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// The gateway's inference series (inferencemetrics.GatewayDefinitions): the
// request duration and first-token histograms, and the tokens engines report
// in their replies' usage, accumulated per deployment beside the request
// counters and flushed with them.
var (
	metricDurationBucket   = inferencemetrics.Gateway("request_duration_ms").Series() + "_bucket"
	metricTTFTBucket       = inferencemetrics.Gateway("ttft_ms").Series() + "_bucket"
	metricTTFTSum          = inferencemetrics.Gateway("ttft_ms").Series() + "_sum"
	metricTTFTCount        = inferencemetrics.Gateway("ttft_ms").Series() + "_count"
	metricPromptTokens     = inferencemetrics.Gateway("prompt_tokens_total").Series()
	metricCompletionTokens = inferencemetrics.Gateway("completion_tokens_total").Series()
	metricCachedTokens     = inferencemetrics.Gateway("cached_tokens_total").Series()
	metricUsageMissing     = inferencemetrics.Gateway("usage_missing_total").Series()
)

// Bucket upper bounds in milliseconds; +Inf is added when emitted. A handful
// per histogram keeps the series a busy deployment writes every flush small
// (plan §7): the quantiles they give are coarse, enough for a panel.
var (
	durationBoundsMS = []float64{1000, 2500, 5000, 10000, 30000, 60000, 120000}
	ttftBoundsMS     = []float64{250, 500, 1000, 2500, 5000, 10000, 30000}
)

// histogram counts observations per bucket; emitted cumulatively.
type histogram struct {
	counts []uint64 // len(bounds)+1, the last one +Inf
	sum    float64
	count  uint64
}

func (h *histogram) observe(bounds []float64, value float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(bounds)+1)
	}
	index := len(bounds)
	for i, bound := range bounds {
		if value <= bound {
			index = i
			break
		}
	}
	h.counts[index]++
	h.sum += value
	h.count++
}

func (h *histogram) bucketSamples(metric string, bounds []float64, labels map[string]string, tms int64) []tsdb.Sample {
	if h.counts == nil {
		return nil
	}
	out := make([]tsdb.Sample, 0, len(h.counts))
	var cumulative uint64
	for i, count := range h.counts {
		cumulative += count
		le := "+Inf"
		if i < len(bounds) {
			le = strconv.FormatFloat(bounds[i], 'f', -1, 64)
		}
		bucketLabels := make(map[string]string, len(labels)+1)
		for k, v := range labels {
			bucketLabels[k] = v
		}
		bucketLabels["le"] = le
		out = append(out, tsdb.Sample{Metric: metric, Labels: bucketLabels, Value: float64(cumulative), TimeMS: tms})
	}
	return out
}

// responseCounters is one deployment's reply measurements, cumulative for the
// life of the process like every other accounting counter.
type responseCounters struct {
	duration histogram // its sum and count are the existing duration series
	ttft     histogram
	// usageSeen marks a deployment that served an operation reporting usage:
	// only those get token series, so an image or audio deployment writes none.
	usageSeen                  bool
	prompt, completion, cached uint64
	usageMissing               uint64
	// fallback holds the fallback histograms (accounting_fallback.go) by
	// gateway metric name, created on a deployment's first observation.
	fallback map[string]*histogram
	// holdWait is how long the requests held for a wake waited
	// (accounting_hold.go).
	holdWait histogram
}

func (r *responseCounters) record(duration time.Duration, stats responseStats, fallbacks []inferencemetrics.GatewayFallback) {
	r.duration.observe(durationBoundsMS, float64(duration)/float64(time.Millisecond))
	r.recordFallbacks(stats, fallbacks)
	if stats.hasFirstToken {
		r.ttft.observe(ttftBoundsMS, float64(stats.firstToken)/float64(time.Millisecond))
	}
	if !stats.observed {
		return
	}
	r.usageSeen = true
	if !stats.usage {
		r.usageMissing++
		return
	}
	r.prompt += stats.prompt
	r.completion += stats.completion
	r.cached += stats.cached
}

func (r *responseCounters) samples(dep string, tms int64) []tsdb.Sample {
	labels := map[string]string{"deployment": dep, "node": serverNodeLabel}
	out := r.duration.bucketSamples(metricDurationBucket, durationBoundsMS, labels, tms)
	if r.ttft.count > 0 {
		out = append(out, r.ttft.bucketSamples(metricTTFTBucket, ttftBoundsMS, labels, tms)...)
		out = append(out,
			tsdb.Sample{Metric: metricTTFTSum, Labels: labels, Value: r.ttft.sum, TimeMS: tms},
			tsdb.Sample{Metric: metricTTFTCount, Labels: labels, Value: float64(r.ttft.count), TimeMS: tms},
		)
	}
	if r.usageSeen {
		out = append(out,
			tsdb.Sample{Metric: metricPromptTokens, Labels: labels, Value: float64(r.prompt), TimeMS: tms},
			tsdb.Sample{Metric: metricCompletionTokens, Labels: labels, Value: float64(r.completion), TimeMS: tms},
			tsdb.Sample{Metric: metricCachedTokens, Labels: labels, Value: float64(r.cached), TimeMS: tms},
			tsdb.Sample{Metric: metricUsageMissing, Labels: labels, Value: float64(r.usageMissing), TimeMS: tms},
		)
	}
	out = append(out, r.holdWaitSamples(labels, tms)...)
	return append(out, r.fallbackSamples(labels, tms)...)
}

// FinishResponse is Finish for a reply the gateway observed: it also counts
// the reply's duration bucket, first token and usage, and the fallback
// histograms the deployment records (replyReading.fallbacks).
func (a *Accountant) FinishResponse(deploymentID uint, status int, duration time.Duration, stats responseStats, fallbacks []inferencemetrics.GatewayFallback) {
	if a == nil {
		return
	}
	a.record(deploymentID, status, false, duration, true, stats, fallbacks)
}
