package monitor

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// WindowStep is the bucket width every window query uses except idle (see
// idleLookback/idleStep in window_idle.go). It mirrors the agent
// collector's tick interval (plan §4.1: "10 s 间隔不变"), which is the
// finest resolution the data actually has.
const WindowStep = 10 * time.Second

// Window holds the statistics ComputeWindow derives for one deployment from
// pre-fetched tsdb series (WindowSeries), over the trailing Span ending at
// Now.
//
// Has* flags let rules distinguish a metric that was queried and found
// genuinely zero from one this agent/engine version never reports at all -
// conflating the two would fire idle/thermal/kv_saturated on deployments
// that simply haven't been instrumented yet (plan §11: "老 agent...信号显示
// '需要升级 agent'，不显示 0").
//
// The *For fields are how rules apply the plan's uniform "持续 >= 60 s"
// persistence requirement (plan §5): each is how long, counting back from
// Now, the named condition has held continuously - see HeldFor in
// aggregate.go and the watermark constants in thresholds.go.
type Window struct {
	Span time.Duration
	Now  time.Time

	// PD is the PD disaggregation part (window_pd.go); zero for others.
	PD PDWindow

	// LatestSampleAt is the newest timestamp among every sample this
	// deployment's WindowSeries carried, across every metric - whether or
	// not that metric ended up populating a Has* field. A deployment with,
	// say, only GPU and KV samples but no token counters still has a
	// meaningful "how fresh is this" answer; callers computing AsOf/Stale
	// (BuildCluster, BuildService) fold this in rather than relying only on
	// metrics that happen to have their own Has* semantics. Zero
	// (IsZero()) means no series carried any sample at all.
	LatestSampleAt time.Time

	HasRunning   bool
	HasWaiting   bool
	HasKVUsage   bool
	HasKVHitRate bool
	HasTokens    bool

	RunningAvg float64
	WaitingAvg float64
	WaitingP95 float64

	// KVUsageMax/Avg are the fraction (0..1) of the engine's KV cache
	// occupied by in-flight requests, maxed/averaged across the
	// deployment's replicas at each bucket then reduced over the window.
	KVUsageMax float64
	KVUsageAvg float64

	// KVHitRate is a fraction (0..1): the vLLM prefix-cache window ratio
	// (kv_hits_total delta / kv_queries_total delta) when those counters
	// exist, else the SGLang instant gauge averaged over the window.
	KVHitRate float64

	PromptTokensPerSec float64
	GenTokensPerSec    float64

	// --- Signals derived from engine histograms (quantile-at-ingest
	// gauges), proxy accounting and the extra GPU/node telemetry.

	HasTTFT    bool
	TTFTP50Sec float64
	TTFTP95Sec float64
	TTFTP99Sec float64

	HasTPOT    bool
	TPOTP95Sec float64
	TPOTP99Sec float64

	HasE2E    bool
	E2EP95Sec float64

	HasQueueTime      bool
	QueueTimeP95Sec   float64
	HasPrefillTime    bool
	PrefillTimeP95Sec float64
	HasDecodeTime     bool
	DecodeTimeP95Sec  float64

	HasPreemptions    bool
	PreemptionsPerSec float64

	// HasErrRate/HasRequestRate come from the same proxy requests_total
	// series (a deployment with any proxy traffic at all has both, or
	// neither); kept as two flags because request_rate and error_rate are
	// two independently nullable wire signals (plan wire contract §6).
	HasRequestRate bool
	RequestsPerSec float64

	HasErrRate bool
	// ErrRate4xx is the plain-4xx (non-429) share of proxy traffic - a
	// client-caused error (bad request, missing model, ...) rather than a
	// server-side failure or a rate limit. No rule persists against it
	// (there is no *AboveThresholdFor for it), but the deployment page's
	// alignment row needs it to classify "client_errors" correctly (see
	// dto.MonitorAlignment's doc comment).
	ErrRate4xx    float64
	ErrRate5xx    float64
	ErrRate429    float64
	ErrRateAsleep float64

	HasInflight bool
	InflightAvg float64
	// InflightLast is the proxy in-flight gauge's own most recent sample in
	// the window (not averaged) - dto.MonitorAlignment.InFlight, a purely
	// informational figure: a request the proxy dispatched and is still
	// holding open counts here even though it never reduces InflightAvg's
	// window-wide average toward zero, and even though (unlike
	// ProxyRequestsDelta below) it was never counted by the proxy's request
	// counters in the first place - see dto.MonitorAlignment's doc comment.
	InflightLast float64
	// InflightMax is the largest value the in-flight gauge took anywhere in
	// the window. dto.MonitorAlignment.DiscrepancyPersistent compares Lost
	// against InflightMax as an upper bound: even crediting every
	// concurrently open request at the window's busiest moment, a Lost
	// that still exceeds InflightMax cannot be explained away by requests
	// merely still streaming, so it is treated as a genuine, persistent
	// discrepancy rather than the ordinary "some requests are still in
	// flight" case.
	InflightMax float64

	// ProxyRequestsDelta/ClientErrorsDelta/ServerErrorsDelta are the
	// gpuworks_proxy_requests_total counter's actual accumulated growth
	// over the window - each class's series walked point-to-point with the
	// same reset-aware delta SumSeriesDelta/computeKVHitRate already apply
	// to gpuworks_infer_kv_queries_total/kv_hits_total - rather than
	// RequestsPerSec/ErrRate*'s rate-then-multiply-by-span approximation
	// (Rate drops a series' first point and Mean smooths tick-to-tick
	// noise, so rate*span and the counter's true growth over the same span
	// can disagree by more than rounding). The alignment row (plan §3.2)
	// needs the exact figure so proxy_requests/client_errors/server_errors
	// and the engine's own success-counter delta are directly comparable;
	// RequestsPerSec/ErrRate4xx/429/5xx/Asleep keep their existing
	// rate-based meaning for the rules/signals that were already built on
	// them (a smoothed instantaneous rate, not a window total).
	ProxyRequestsDelta float64
	// ClientErrorsDelta is the client's own fault: the "4xx" and "429"
	// classes' combined counter delta - mirrors ErrRate4xx+ErrRate429's
	// class split, see ErrRate4xx's doc comment.
	ClientErrorsDelta float64
	// ServerErrorsDelta is the server's own fault: the "5xx" and
	// "asleep_503" classes' combined counter delta - mirrors
	// ErrRate5xx+ErrRateAsleep's class split.
	ServerErrorsDelta float64
}

// WindowParams bundles ComputeWindow's non-series inputs: the deployment's
// current instance/replica state and the clock/span it is being computed
// for. Grouped into one struct (rather than five positional parameters)
// because every caller already has all five values sitting together before
// calling ComputeWindow.
type WindowParams struct {
	Now  time.Time
	Span time.Duration
}

// ComputeWindow reduces a deployment's pre-fetched WindowSeries (built by
// FetchWindowSeries for a single deployment, or GroupWindowSeries from a
// fleet-wide fetch) into a Window. It performs no I/O: every tsdb query
// already happened in whichever Fetch* function produced series, which is
// exactly what lets BuildCluster fetch every metric once for the whole
// fleet and call this once per deployment instead of querying tsdb once per
// deployment per metric (plan §8 second phase capacity requirement).
func ComputeWindow(series WindowSeries, p WindowParams) Window {
	w := Window{Span: p.Span, Now: p.Now}

	var latestSeen int64
	haveLatestSeen := false
	noteLatest := func(ss []tsdb.Series) {
		if pt, ok := LatestValue(ss); ok {
			if !haveLatestSeen || pt.TimeMS > latestSeen {
				latestSeen, haveLatestSeen = pt.TimeMS, true
			}
		}
	}
	for _, ss := range [][]tsdb.Series{
		series.Running, series.Waiting, series.KVUsage, series.KVQueries, series.KVHits, series.KVInstant,
		series.PromptTokens, series.GenTokens, series.TTFT, series.TPOT, series.E2E, series.QueueTime, series.PrefillTime, series.DecodeTime,
		series.Preemptions, series.ProxyRequestsTotal, series.ProxyDurationSum, series.ProxyInflight,
	} {
		noteLatest(ss)
	}
	if haveLatestSeen {
		w.LatestSampleAt = time.UnixMilli(latestSeen).UTC()
	}

	var runningTotal []tsdb.Series
	if len(series.Running) > 0 {
		runningTotal = SumBy(series.Running, "")
	}
	w.HasRunning = len(runningTotal) > 0 && len(runningTotal[0].Points) > 0
	if w.HasRunning {
		w.RunningAvg = Mean(Values(runningTotal[0]))
	}

	var waitingTotal []tsdb.Series
	if len(series.Waiting) > 0 {
		waitingTotal = SumBy(series.Waiting, "")
	}
	w.HasWaiting = len(waitingTotal) > 0 && len(waitingTotal[0].Points) > 0
	if w.HasWaiting {
		values := Values(waitingTotal[0])
		w.WaitingAvg = Mean(values)
		w.WaitingP95 = Quantile(values, 0.95)
	}

	var kvMax []tsdb.Series
	if len(series.KVUsage) > 0 {
		kvMax = MaxBy(series.KVUsage, "")
	}
	w.HasKVUsage = len(kvMax) > 0 && len(kvMax[0].Points) > 0
	if w.HasKVUsage {
		values := Values(kvMax[0])
		w.KVUsageMax = MaxOf(values)
		w.KVUsageAvg = Mean(values)
	}

	computeKVHitRate(&w, series.KVQueries, series.KVHits, series.KVInstant)

	promptRate := Rate(series.PromptTokens)
	genRate := Rate(series.GenTokens)
	w.HasTokens = len(series.PromptTokens) > 0 || len(series.GenTokens) > 0
	if promptRateTotal := SumBy(promptRate, ""); len(promptRateTotal) > 0 {
		w.PromptTokensPerSec = Mean(Values(promptRateTotal[0]))
	}
	if genRateTotal := SumBy(genRate, ""); len(genRateTotal) > 0 {
		w.GenTokensPerSec = Mean(Values(genRateTotal[0]))
	}

	computeInferHistograms(&w, series)
	computeProxySignals(&w, series)
	return w
}

// computeKVHitRate prefers the vLLM window-ratio counters (a true ratio over
// the whole span) and falls back to the SGLang instant gauge, averaged, when
// the counters are absent. The result is clamped to [0,1]: a hit rate above
// 1 or below 0 is never meaningful and would otherwise leak through from a
// pathological counter series.
func computeKVHitRate(w *Window, queries, hits, instant []tsdb.Series) {
	if len(queries) > 0 && len(hits) > 0 {
		queryDelta := SumSeriesDelta(queries)
		if queryDelta > 0 {
			w.KVHitRate = clamp01(SumSeriesDelta(hits) / queryDelta)
			w.HasKVHitRate = true
			return
		}
	}
	if len(instant) == 0 {
		return
	}
	avg := AvgBy(instant, "")
	if len(avg) == 0 || len(avg[0].Points) == 0 {
		return
	}
	w.KVHitRate = clamp01(Mean(Values(avg[0])))
	w.HasKVHitRate = true
}

// kvHitRateByInstance is computeKVHitRate per replica and per bucket, for
// the kv chart: a replica with prefix-cache counters (vLLM) gets hits/queries
// over each step, the same source preference as the window's single number;
// one without them (SGLang) falls back to its instant gauge. A step with no
// queries has no hit rate, so it leaves a gap rather than a false zero.
func kvHitRateByInstance(queries, hits, instant []tsdb.Series) []tsdb.Series {
	hitRates := map[string]map[int64]float64{}
	for _, s := range SumBy(Rate(hits), "instance") {
		byTS := make(map[int64]float64, len(s.Points))
		for _, p := range s.Points {
			byTS[p.TimeMS] = p.Value
		}
		hitRates[s.Labels["instance"]] = byTS
	}
	var out []tsdb.Series
	fromCounters := map[string]bool{}
	for _, s := range SumBy(Rate(queries), "instance") {
		instance := s.Labels["instance"]
		hitsAt, ok := hitRates[instance]
		if !ok {
			continue
		}
		points := make([]tsdb.Point, 0, len(s.Points))
		for _, p := range s.Points {
			hit, ok := hitsAt[p.TimeMS]
			if !ok || p.Value <= 0 {
				continue
			}
			points = append(points, tsdb.Point{TimeMS: p.TimeMS, Value: clamp01(hit / p.Value)})
		}
		fromCounters[instance] = true
		out = append(out, tsdb.Series{Labels: s.Labels, Points: points})
	}
	for _, s := range AvgBy(instant, "instance") {
		if !fromCounters[s.Labels["instance"]] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Labels["instance"] < out[j].Labels["instance"] })
	return out
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// SumSeriesDelta sums each series' accumulated counter delta over its whole
// point sequence, walking consecutive points with the same reset handling as
// Rate (resetAwareStepDelta): a decrease between any two consecutive points
// is a counter reset, not a negative contribution. Looking only at the
// first and last point would miss a reset that happened and partially
// recovered in the middle of the window - the delta would look like normal
// (possibly small, or even negative) growth instead of two separate
// accumulations. Exported (unlike this file's other helpers) because
// monitorapi's alignment row needs the identical reset-aware delta over a
// series it queries itself (the engine's own success counter, which is not
// part of WindowSeries - see monitorapi.buildAlignment).
func SumSeriesDelta(series []tsdb.Series) float64 {
	total := 0.0
	for _, s := range series {
		for i := 1; i < len(s.Points); i++ {
			total += resetAwareStepDelta(s.Points[i-1].Value, s.Points[i].Value)
		}
	}
	return total
}

func toSeries(points []tsdb.Point) []tsdb.Series {
	if len(points) == 0 {
		return nil
	}
	return []tsdb.Series{{Points: points}}
}
