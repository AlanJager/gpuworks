package monitor

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file is the PD disaggregation part of the window (ADR 0028). Every
// user request of a PD deployment is decoded exactly once, by a decode
// instance, while a prefill instance also sees it as a one-token request.
// So the engine signals the ordinary rules read (running, waiting, tokens,
// TPOT, E2E) are taken from the decode instances alone, and the PD fields
// below come from the prefill instances and the gateway:
//
//   - PD effectiveness: of the prompt tokens the decode instances received,
//     the share that arrived by KV transfer rather than local compute or the
//     local prefix cache. It is read on the decode side: NIXL is pulled by the
//     decoder, the prefill side's transfer counters stay 0.
//   - fallback rate: requests the gateway sent to a decoder alone, over all
//     requests.
//   - prefill stage: the gateway's measured time for the prefill stage,
//     because a decoder's own TTFT does not contain the prefill.
//   - KV transfer: the engine's own transfer histograms, read on the side
//     that pulls the KV (the decode instances), when the PD mode's engine
//     exports them (NIXL); vllm-ascend's Mooncake connector exports none.

// Role names as instances carry them (pdmode.Role*; duplicated as the fixed
// wire vocabulary, this package imports no server package).
const (
	roleDecode  = "decode"
	rolePrefill = "prefill"
)

const (
	metricPromptExternal     = "gpuworks_infer_prompt_tokens_external_total"
	metricPromptLocalCompute = "gpuworks_infer_prompt_tokens_local_compute_total"
	metricPromptCacheHit     = "gpuworks_infer_prompt_tokens_cache_hit_total"
	metricPDFallbackTotal    = "gpuworks_proxy_pd_fallback_total"
	metricPDPrefillSum       = "gpuworks_proxy_pd_prefill_duration_ms_sum"
	metricPDPrefillCount     = "gpuworks_proxy_pd_prefill_duration_ms_count"
	metricKVTransferSeconds  = "gpuworks_infer_nixl_transfer_seconds"
	metricKVTransferBytes    = "gpuworks_infer_nixl_transfer_bytes"
	metricKVTransferFailed   = "gpuworks_infer_nixl_failed_transfers_total"
	metricKVTransferExpired  = "gpuworks_infer_nixl_kv_expired_total"
)

// PDWindow is the PD part of a Window; IsPD is false for every other
// deployment and every other field is then zero.
type PDWindow struct {
	IsPD bool

	// HasEffectiveness is set once the decode instances received prompt
	// tokens in the window; Effectiveness is the 0..1 external share.
	HasEffectiveness bool
	Effectiveness    float64

	// HasFallback is set when the gateway handled requests in the window;
	// FallbackRate is fallbacks over requests (0..1), Fallbacks the count
	// by reason.
	HasFallback  bool
	FallbackRate float64
	Fallbacks    map[string]float64

	// HasPrefillStage is set when the gateway ran prefill stages in the
	// window; PrefillStageAvgSec is their mean duration.
	HasPrefillStage    bool
	PrefillStageAvgSec float64

	// PrefillWaitingAvg is the prefill instances' mean queue length.
	HasPrefillWaiting bool
	PrefillWaitingAvg float64

	// AvgPromptTokens is the decode side's mean prompt length per request;
	// zero when unknown.
	AvgPromptTokens float64

	// KV transfer, from the decode instances' transfer histograms.
	// TransferExposed says the PD mode's engine exports them at all;
	// HasTransfer that transfers happened in the window. Per transfer: the
	// mean and the p95 duration, the mean size, and the rate the bytes moved
	// while transferring; Failed counts the decoders' failed transfers and
	// Expired the requests whose KV a prefill instance released because no
	// decoder pulled it before its lease ran out (vLLM counts these on the
	// prefill side; a decoder holding the request renews the lease).
	TransferExposed     bool
	HasTransfer         bool
	TransferAvgSec      float64
	TransferP95Sec      float64
	HasTransferP95      bool
	TransferAvgBytes    float64
	TransferBytesPerSec float64
	TransferFailed      float64
	TransferExpired     float64
}

// PDSeries is what a PD deployment's window reads beyond WindowSeries.
type PDSeries struct {
	External, LocalCompute, CacheHit []tsdb.Series
	Fallbacks                        []tsdb.Series
	PrefillSum, PrefillCount         []tsdb.Series
	// PrefillWaiting is the prefill instances' queue gauge.
	PrefillWaiting []tsdb.Series
	// KV transfer histograms and counters, narrowed to the decode
	// instances (DecodeWindowSeries) - except TransferExpired, which only
	// the prefill instances count.
	TransferSeconds, TransferSecondsSum, TransferSecondsCount []tsdb.Series
	TransferBytesSum, TransferBytesCount                      []tsdb.Series
	TransferFailed, TransferExpired                           []tsdb.Series
}

// FetchPDSeries queries one PD deployment's extra series. It is per
// deployment rather than part of the fleet index: PD deployments are few,
// and every ordinary one then costs nothing.
func FetchPDSeries(store tsdb.Store, deploymentID uint, now, spanStart time.Time, stepMS int64) (PDSeries, error) {
	matcher := map[string]string{"deployment": depKeyString(deploymentID)}
	startMS, endMS := spanStart.UnixMilli(), now.UnixMilli()
	var out PDSeries
	for metric, dst := range map[string]*[]tsdb.Series{
		metricPromptExternal:               &out.External,
		metricPromptLocalCompute:           &out.LocalCompute,
		metricPromptCacheHit:               &out.CacheHit,
		metricPDFallbackTotal:              &out.Fallbacks,
		metricPDPrefillSum:                 &out.PrefillSum,
		metricPDPrefillCount:               &out.PrefillCount,
		metricKVTransferSeconds:            &out.TransferSeconds,
		metricKVTransferSeconds + "_sum":   &out.TransferSecondsSum,
		metricKVTransferSeconds + "_count": &out.TransferSecondsCount,
		metricKVTransferBytes + "_sum":     &out.TransferBytesSum,
		metricKVTransferBytes + "_count":   &out.TransferBytesCount,
		metricKVTransferFailed:             &out.TransferFailed,
		metricKVTransferExpired:            &out.TransferExpired,
	} {
		series, err := store.QueryRange(metric, matcher, startMS, endMS, stepMS)
		if err != nil {
			return PDSeries{}, err
		}
		*dst = series
	}
	return out, nil
}

// splitByRole keeps the series whose instance label belongs to one role.
func splitByRole(series []tsdb.Series, uuids map[string]bool) []tsdb.Series {
	var out []tsdb.Series
	for _, s := range series {
		if uuids[s.Labels["instance"]] {
			out = append(out, s)
		}
	}
	return out
}

// roleUUIDs indexes a role's instance UUIDs.
func roleUUIDs(instances []InstanceRef, role string) map[string]bool {
	out := map[string]bool{}
	for _, inst := range instances {
		if inst.Role == role {
			out[inst.UUID] = true
		}
	}
	return out
}

// DecodeWindowSeries narrows a PD deployment's engine series to its decode
// instances, which see each user request exactly once, and returns the
// prefill instances' waiting gauge for the PD fields. Proxy, GPU and node
// series stay as they are: they describe the deployment as a whole.
func DecodeWindowSeries(series WindowSeries, instances []InstanceRef, pd *PDSeries) WindowSeries {
	decode, prefill := roleUUIDs(instances, roleDecode), roleUUIDs(instances, rolePrefill)
	if pd != nil {
		pd.PrefillWaiting = splitByRole(series.Waiting, prefill)
		pd.External = splitByRole(pd.External, decode)
		pd.LocalCompute = splitByRole(pd.LocalCompute, decode)
		pd.CacheHit = splitByRole(pd.CacheHit, decode)
		for _, ss := range []*[]tsdb.Series{&pd.TransferSeconds, &pd.TransferSecondsSum, &pd.TransferSecondsCount,
			&pd.TransferBytesSum, &pd.TransferBytesCount, &pd.TransferFailed} {
			*ss = splitByRole(*ss, decode)
		}
		pd.TransferExpired = splitByRole(pd.TransferExpired, prefill)
	}
	only := func(ss []tsdb.Series) []tsdb.Series { return splitByRole(ss, decode) }
	series.Running, series.Waiting = only(series.Running), only(series.Waiting)
	series.KVUsage, series.KVQueries, series.KVHits, series.KVInstant = only(series.KVUsage), only(series.KVQueries), only(series.KVHits), only(series.KVInstant)
	series.PromptTokens, series.GenTokens = only(series.PromptTokens), only(series.GenTokens)
	series.TTFT, series.TPOT, series.E2E = only(series.TTFT), only(series.TPOT), only(series.E2E)
	series.QueueTime, series.PrefillTime, series.DecodeTime = only(series.QueueTime), only(series.PrefillTime), only(series.DecodeTime)
	series.Preemptions = only(series.Preemptions)
	return series
}

// ComputePDWindow fills the PD fields of an already computed window.
func ComputePDWindow(w *Window, pd PDSeries, transferExposed bool) {
	w.PD = PDWindow{IsPD: true, TransferExposed: transferExposed}
	computePDTransfer(&w.PD, pd)
	external := SumSeriesDelta(pd.External)
	total := external + SumSeriesDelta(pd.LocalCompute) + SumSeriesDelta(pd.CacheHit)
	if total > 0 {
		w.PD.HasEffectiveness = true
		w.PD.Effectiveness = external / total
	}
	if w.ProxyRequestsDelta > 0 {
		w.PD.HasFallback = true
		w.PD.Fallbacks = map[string]float64{}
		fallbacks := 0.0
		for _, s := range pd.Fallbacks {
			n := SumSeriesDelta([]tsdb.Series{s})
			w.PD.Fallbacks[s.Labels["reason"]] += n
			fallbacks += n
		}
		w.PD.FallbackRate = fallbacks / w.ProxyRequestsDelta
		if total > 0 {
			w.PD.AvgPromptTokens = total / w.ProxyRequestsDelta
		}
	}
	if count := SumSeriesDelta(pd.PrefillCount); count > 0 {
		w.PD.HasPrefillStage = true
		w.PD.PrefillStageAvgSec = SumSeriesDelta(pd.PrefillSum) / count / 1000
	}
	if waiting := SumBy(pd.PrefillWaiting, ""); len(waiting) > 0 && len(waiting[0].Points) > 0 {
		w.PD.HasPrefillWaiting = true
		w.PD.PrefillWaitingAvg = Mean(Values(waiting[0]))
	}
}

// pdSeriesFor narrows a PD deployment's series to its decode instances in
// place and fetches its PD series; nil for any other deployment.
func pdSeriesFor(store tsdb.Store, dep DeploymentRef, instances []InstanceRef, series *WindowSeries, now time.Time) (*PDSeries, error) {
	if !dep.PD {
		return nil, nil
	}
	pd, err := FetchPDSeries(store, dep.ID, now, now.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		return nil, err
	}
	*series = DecodeWindowSeries(*series, instances, &pd)
	return &pd, nil
}

// computePDTransfer fills the KV transfer fields from the decode instances'
// transfer histograms, when the engine exports them.
func computePDTransfer(w *PDWindow, pd PDSeries) {
	if !w.TransferExposed {
		return
	}
	count := SumSeriesDelta(pd.TransferSecondsCount)
	if count <= 0 {
		return
	}
	seconds := SumSeriesDelta(pd.TransferSecondsSum)
	w.HasTransfer = true
	w.TransferAvgSec = seconds / count
	w.HasTransferP95, w.TransferP95Sec = reduceP95(pd.TransferSeconds)
	if n := SumSeriesDelta(pd.TransferBytesCount); n > 0 {
		bytes := SumSeriesDelta(pd.TransferBytesSum)
		w.TransferAvgBytes = bytes / n
		if seconds > 0 {
			w.TransferBytesPerSec = bytes / seconds
		}
	}
	w.TransferFailed = SumSeriesDelta(pd.TransferFailed)
	w.TransferExpired = SumSeriesDelta(pd.TransferExpired)
}
