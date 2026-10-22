package monitor

import (
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Signal sources (ADR 0039, docs/reference/inference-metrics.md). A panel
// signal comes from the engine's own series when the engine reports them and
// from the gateway's otherwise, so a deployment whose engine exports nothing
// (Strata has no /metrics) still has a TTFT, an end-to-end latency, token
// throughput and a concurrency, and one whose engine exports no TPOT or phase
// times (TensorFold) gets the gateway's TPOT and the phase times its replies
// report (inferencemetrics.GatewayFallbacksFor). This file is the one place that decides
// which series provide which signal: once a WindowSeries is fetched,
// resolveSources fills each engine field the engine left empty from the
// gateway, converted to the engine field's unit, and records the source.
// Everything downstream - the window, the rules, the charts - reads the
// engine fields and never needs to know.
//
// The engine wins whenever it has data, because its numbers are the engine's
// own: the gateway's TTFT also counts the queue and the network, and its
// in-flight count is running and waiting together.

// Signal names a panel signal that may come from more than one source.
type Signal string

const (
	SignalTTFT             Signal = "ttft"
	SignalE2E              Signal = "e2e"
	SignalPromptTokens     Signal = "prompt_tokens"
	SignalGenerationTokens Signal = "generation_tokens"
	SignalRunning          Signal = "running"
	SignalTPOT             Signal = "tpot"
	SignalQueueTime        Signal = "queue_time"
	SignalPrefillTime      Signal = "prefill_time"
	SignalDecodeTime       Signal = "decode_time"
)

// Source is where a signal's series came from.
type Source string

const (
	SourceEngine  Source = "engine"
	SourceGateway Source = "gateway"
)

var (
	metricGatewayTTFT             = inferencemetrics.Gateway("ttft_ms").Series()
	metricGatewayDuration         = inferencemetrics.Gateway("request_duration_ms").Series()
	metricGatewayPromptTokens     = inferencemetrics.Gateway("prompt_tokens_total").Series()
	metricGatewayCompletionTokens = inferencemetrics.Gateway("completion_tokens_total").Series()
	metricGatewayTPOT             = inferencemetrics.Gateway("tpot_ms").Series()
	metricGatewayQueueTime        = inferencemetrics.Gateway("reported_queue_ms").Series()
	metricGatewayPrefillTime      = inferencemetrics.Gateway("reported_prefill_ms").Series()
	metricGatewayDecodeTime       = inferencemetrics.Gateway("reported_decode_ms").Series()
)

// GatewaySeries are the gateway's inference series of one deployment that
// can stand in for an engine's. TTFT, Duration, TPOT and the phase times are
// quantile gauges in milliseconds.
type GatewaySeries struct {
	TTFT, Duration                 []tsdb.Series
	PromptTokens, CompletionTokens []tsdb.Series
	TPOT                           []tsdb.Series
	QueueTime, PrefillTime         []tsdb.Series
	DecodeTime                     []tsdb.Series
}

// gatewayFields pairs each gateway series with its GatewaySeries field: the
// one list the per-deployment and fleet queries and the fleet index walk.
var gatewayFields = []struct {
	metric string
	field  func(*GatewaySeries) *[]tsdb.Series
}{
	{metricGatewayTTFT, func(g *GatewaySeries) *[]tsdb.Series { return &g.TTFT }},
	{metricGatewayDuration, func(g *GatewaySeries) *[]tsdb.Series { return &g.Duration }},
	{metricGatewayPromptTokens, func(g *GatewaySeries) *[]tsdb.Series { return &g.PromptTokens }},
	{metricGatewayCompletionTokens, func(g *GatewaySeries) *[]tsdb.Series { return &g.CompletionTokens }},
	{metricGatewayTPOT, func(g *GatewaySeries) *[]tsdb.Series { return &g.TPOT }},
	{metricGatewayQueueTime, func(g *GatewaySeries) *[]tsdb.Series { return &g.QueueTime }},
	{metricGatewayPrefillTime, func(g *GatewaySeries) *[]tsdb.Series { return &g.PrefillTime }},
	{metricGatewayDecodeTime, func(g *GatewaySeries) *[]tsdb.Series { return &g.DecodeTime }},
}

// signalSource is one signal's candidates: the engine field it fills and the
// gateway series that stands in for it, with the factor from the gateway's
// unit to the engine field's.
type signalSource struct {
	signal  Signal
	engine  func(*WindowSeries) *[]tsdb.Series
	gateway func(*WindowSeries) []tsdb.Series
	scale   float64
}

var signalCatalog = []signalSource{
	{SignalTTFT, func(ws *WindowSeries) *[]tsdb.Series { return &ws.TTFT }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.TTFT }, 0.001},
	{SignalE2E, func(ws *WindowSeries) *[]tsdb.Series { return &ws.E2E }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.Duration }, 0.001},
	{SignalPromptTokens, func(ws *WindowSeries) *[]tsdb.Series { return &ws.PromptTokens }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.PromptTokens }, 1},
	{SignalGenerationTokens, func(ws *WindowSeries) *[]tsdb.Series { return &ws.GenTokens }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.CompletionTokens }, 1},
	{SignalRunning, func(ws *WindowSeries) *[]tsdb.Series { return &ws.Running }, func(ws *WindowSeries) []tsdb.Series { return ws.ProxyInflight }, 1},
	{SignalTPOT, func(ws *WindowSeries) *[]tsdb.Series { return &ws.TPOT }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.TPOT }, 0.001},
	{SignalQueueTime, func(ws *WindowSeries) *[]tsdb.Series { return &ws.QueueTime }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.QueueTime }, 0.001},
	{SignalPrefillTime, func(ws *WindowSeries) *[]tsdb.Series { return &ws.PrefillTime }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.PrefillTime }, 0.001},
	{SignalDecodeTime, func(ws *WindowSeries) *[]tsdb.Series { return &ws.DecodeTime }, func(ws *WindowSeries) []tsdb.Series { return ws.Gateway.DecodeTime }, 0.001},
}

// resolveSources fills every catalog signal the engine left without data
// from the gateway and records each signal's source. A signal neither
// provides is absent from Sources.
func (ws *WindowSeries) resolveSources() {
	ws.Sources = map[Signal]Source{}
	for _, candidate := range signalCatalog {
		dst := candidate.engine(ws)
		if hasPoints(*dst) {
			ws.Sources[candidate.signal] = SourceEngine
			continue
		}
		if gateway := candidate.gateway(ws); hasPoints(gateway) {
			*dst = scaleSeries(gateway, candidate.scale)
			ws.Sources[candidate.signal] = SourceGateway
		}
	}
}

func hasPoints(series []tsdb.Series) bool {
	for _, s := range series {
		if len(s.Points) > 0 {
			return true
		}
	}
	return false
}

// scaleSeries copies series with every value multiplied by factor.
func scaleSeries(series []tsdb.Series, factor float64) []tsdb.Series {
	if factor == 1 {
		return series
	}
	out := make([]tsdb.Series, len(series))
	for i, s := range series {
		points := make([]tsdb.Point, len(s.Points))
		for j, p := range s.Points {
			points[j] = tsdb.Point{TimeMS: p.TimeMS, Value: p.Value * factor}
		}
		out[i] = tsdb.Series{Labels: s.Labels, Points: points}
	}
	return out
}

// queryGatewaySeries fetches one deployment's GatewaySeries.
func queryGatewaySeries(store tsdb.Store, depKey string, startMS, endMS, stepMS int64) (GatewaySeries, error) {
	return queryGatewayFields(store, map[string]string{"deployment": depKey}, startMS, endMS, stepMS)
}

// queryFleetGatewaySeries fetches every deployment's GatewaySeries in one
// fleet-wide query per series, for BuildCluster to slice per deployment.
func queryFleetGatewaySeries(store tsdb.Store, startMS, endMS, stepMS int64) (GatewaySeries, error) {
	return queryGatewayFields(store, nil, startMS, endMS, stepMS)
}

func queryGatewayFields(store tsdb.Store, matcher map[string]string, startMS, endMS, stepMS int64) (GatewaySeries, error) {
	var out GatewaySeries
	for _, f := range gatewayFields {
		series, err := store.QueryRange(f.metric, matcher, startMS, endMS, stepMS)
		if err != nil {
			return GatewaySeries{}, err
		}
		*f.field(&out) = series
	}
	return out, nil
}

// gatewayIndex slices fleet-wide GatewaySeries by deployment.
type gatewayIndex map[string]GatewaySeries

func indexGatewaySeries(fleet GatewaySeries) gatewayIndex {
	idx := gatewayIndex{}
	for _, f := range gatewayFields {
		for _, s := range *f.field(&fleet) {
			dep := s.Labels["deployment"]
			if dep == "" {
				continue
			}
			g := idx[dep]
			*f.field(&g) = append(*f.field(&g), s)
			idx[dep] = g
		}
	}
	return idx
}
