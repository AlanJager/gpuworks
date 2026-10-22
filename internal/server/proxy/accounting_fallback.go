package proxy

import (
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// The fallback histograms (inferencemetrics.GatewayFallbacksFor) stand in for
// engine histograms a deployment's engine does not export: a TPOT the gateway
// measures, and the phase times the engine reports inside its replies. The
// route table decides which ones a deployment records (replyReading), so a
// vLLM deployment, which exports all four itself, writes none of them.

// fallbackBoundsMS are each fallback histogram's bucket upper bounds in
// milliseconds; +Inf is added when emitted. As few as the other gateway
// histograms, for the same reason: every flush writes them.
var fallbackBoundsMS = map[string][]float64{
	"tpot_ms":             {5, 10, 20, 35, 50, 75, 100, 200},
	"reported_queue_ms":   {10, 50, 100, 500, 1000, 5000, 30000},
	"reported_prefill_ms": {50, 100, 250, 500, 1000, 2500, 5000, 15000},
	"reported_decode_ms":  {1000, 2500, 5000, 10000, 30000, 60000, 120000},
}

// fallbackSeries are the stored names of one fallback histogram.
type fallbackSeries struct{ bucket, sum, count string }

var fallbackSeriesByName = func() map[string]fallbackSeries {
	out := map[string]fallbackSeries{}
	for _, fallback := range inferencemetrics.GatewayFallbacks() {
		base := inferencemetrics.Gateway(fallback.Gateway).Series()
		out[fallback.Gateway] = fallbackSeries{bucket: base + "_bucket", sum: base + "_sum", count: base + "_count"}
	}
	return out
}()

// fallbackValueMS is what one reply contributes to a fallback histogram, if
// anything: the engine's own figure for a request stat, the gateway's TPOT
// otherwise.
func fallbackValueMS(fallback inferencemetrics.GatewayFallback, stats responseStats) (float64, bool) {
	if fallback.Stat == "" {
		return gatewayTPOTMS(stats)
	}
	seconds, ok := stats.engine[fallback.Stat]
	if !ok || seconds < 0 {
		return 0, false
	}
	return seconds * 1000, true
}

// gatewayTPOTMS is a streamed reply's time per output token as the gateway
// sees it: from the first generated token to the last text event, over the
// tokens after the first. It needs the reply's usage, since an event may
// carry several tokens (speculative decoding sends a round's tokens at once),
// and at least two tokens to have an interval at all.
func gatewayTPOTMS(stats responseStats) (float64, bool) {
	if !stats.hasFirstToken || !stats.usage || stats.completion < 2 || stats.lastEvent <= stats.firstToken {
		return 0, false
	}
	return float64(stats.lastEvent-stats.firstToken) / float64(time.Millisecond) / float64(stats.completion-1), true
}

// recordFallbacks observes one reply into the deployment's fallback
// histograms.
func (r *responseCounters) recordFallbacks(stats responseStats, fallbacks []inferencemetrics.GatewayFallback) {
	for _, fallback := range fallbacks {
		value, ok := fallbackValueMS(fallback, stats)
		if !ok {
			continue
		}
		if r.fallback == nil {
			r.fallback = map[string]*histogram{}
		}
		h := r.fallback[fallback.Gateway]
		if h == nil {
			h = &histogram{}
			r.fallback[fallback.Gateway] = h
		}
		h.observe(fallbackBoundsMS[fallback.Gateway], value)
	}
}

// fallbackSamples emits every fallback histogram that has observations, in
// the vocabulary's order.
func (r *responseCounters) fallbackSamples(labels map[string]string, tms int64) []tsdb.Sample {
	var out []tsdb.Sample
	for _, fallback := range inferencemetrics.GatewayFallbacks() {
		h := r.fallback[fallback.Gateway]
		if h == nil || h.count == 0 {
			continue
		}
		names := fallbackSeriesByName[fallback.Gateway]
		out = append(out, h.bucketSamples(names.bucket, fallbackBoundsMS[fallback.Gateway], labels, tms)...)
		out = append(out,
			tsdb.Sample{Metric: names.sum, Labels: labels, Value: h.sum, TimeMS: tms},
			tsdb.Sample{Metric: names.count, Labels: labels, Value: float64(h.count), TimeMS: tms},
		)
	}
	return out
}
