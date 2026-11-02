package monitorapi

import (
	"context"
	"math"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// chartMetricRename maps monitor.ChartSeries.Name (a plain internal word) to
// the wire contract's exact labels.metric value (plan §6): the kv chart's
// "_pct" suffix and the throughput chart's "tok_s" abbreviation. Series
// whose Name is not in this table (concurrency's "running"/"waiting",
// latency/stage_time/preemptions, all currently empty) pass through
// unchanged - their monitor-side names already match the contract.
var chartMetricRename = map[string]string{
	"usage":                     "usage_pct",
	"hit_rate":                  "hit_rate_pct",
	"prompt_tokens_per_sec":     "prompt_tok_s",
	"generation_tokens_per_sec": "generation_tok_s",
}

// buildDeployment assembles the GET /api/v1/monitor/deployments/{id}
// response (plan §3.2). It looks the deployment up in Inventory itself
// (rather than trusting monitor.BuildService's own not-found error, an
// unexported type this package cannot type-assert against) so a missing id
// maps cleanly to Deps.NotFound and, from there, to the endpoint's 404.
func (s *Service) buildDeployment(ctx context.Context, id uint, rng time.Duration) (dto.MonitorDeployment, error) {
	now := s.deps.now()
	inv, err := s.deps.Inventory(ctx)
	if err != nil {
		return dto.MonitorDeployment{}, err
	}
	dep, ok := findDeployment(inv.Deployments, id)
	if !ok {
		return dto.MonitorDeployment{}, s.deps.NotFound
	}
	deps := s.depsFor(inv, now)
	snap, err := monitor.BuildService(ctx, deps, id, rng)
	if err != nil {
		return dto.MonitorDeployment{}, err
	}

	var instances []monitor.InstanceRef
	for _, inst := range inv.Instances {
		if inst.DeploymentID == id {
			instances = append(instances, inst)
		}
	}
	nodeNames := nodeNameIndex(inv.Nodes)
	instanceState := make(map[string]string, len(instances))
	for _, inst := range instances {
		instanceState[inst.UUID] = replicaState(inst)
	}

	alignment, err := s.buildAlignment(ctx, id, instances, now, snap.Window)
	if err != nil {
		return dto.MonitorDeployment{}, err
	}
	logInstances, err := s.logInstances(ctx, id, instances, now, rng)
	if err != nil {
		return dto.MonitorDeployment{}, err
	}

	return dto.MonitorDeployment{
		AsOf:  timeMillis(snap.AsOf),
		Stale: snap.Stale,
		Deployment: dto.MonitorDeploymentSummary{
			ID: dep.ID, Name: dep.Name, Engine: dep.Engine, Placement: deploymentPlacement(dep, instances, nodeNames),
			Replicas: dto.MonitorReplicaCount{Ready: monitor.ReadyReplicas(dep, instances), Desired: dep.ReplicasDesired},
		},
		Alignment:     alignment,
		TTFTBreakdown: ttftBreakdownDTO(snap.Window),
		Charts:        chartsDTO(snap.Charts),
		Replicas:      replicaRowsDTO(snap.Replicas, instanceState),
		LogInstances:  logInstances,
		PD:            s.withPrefillStageP95(ctx, id, now, pdDTO(snap.Window.PD)),
		Sources:       sourcesDTO(snap.Sources),
		Health:        s.Health(id, dep.HealthCheck, dep.HealthCheckThresholdSec),
	}, nil
}

// pdDTO is the PD summary of a window; nil for an ordinary deployment.
func pdDTO(pd monitor.PDWindow) *dto.MonitorPD {
	if !pd.IsPD {
		return nil
	}
	out := &dto.MonitorPD{
		KVTransfer: dto.MonitorPDKVTransfer{
			Exposed:  pd.TransferExposed,
			AvgMs:    collectedFloatPtr(pd.HasTransfer, round1(pd.TransferAvgSec*1000)),
			P95Ms:    collectedFloatPtr(pd.HasTransferP95, round1(pd.TransferP95Sec*1000)),
			AvgMB:    collectedFloatPtr(pd.HasTransfer && pd.TransferAvgBytes > 0, round1(pd.TransferAvgBytes/1e6)),
			MBPerSec: collectedFloatPtr(pd.HasTransfer && pd.TransferBytesPerSec > 0, round1(pd.TransferBytesPerSec/1e6)),
			Failed:   collectedFloatPtr(pd.HasTransfer, pd.TransferFailed),
			Expired:  collectedFloatPtr(pd.HasTransfer, pd.TransferExpired),
		},
		EffectivenessPct:  collectedFloatPtr(pd.HasEffectiveness, round1(pd.Effectiveness*100)),
		FallbackRatePct:   collectedFloatPtr(pd.HasFallback, round1(pd.FallbackRate*100)),
		Fallbacks:         map[string]float64{},
		PrefillStageAvgMs: collectedFloatPtr(pd.HasPrefillStage, round1(pd.PrefillStageAvgSec*1000)),
		PrefillWaitingAvg: collectedFloatPtr(pd.HasPrefillWaiting, round1(pd.PrefillWaitingAvg)),
		AvgPromptTokens:   collectedFloatPtr(pd.AvgPromptTokens > 0, round1(pd.AvgPromptTokens)),
	}
	for reason, n := range pd.Fallbacks {
		out.Fallbacks[reason] = n
	}
	return out
}

// maxLogInstances bounds MonitorDeployment.LogInstances - the same ceiling
// logstore.Filter.Instances enforces (internal/server/logstore/logstore.go),
// duplicated here rather than imported because monitorapi is a leaf package
// that never depends on the store-adjacent logstore package (see this
// package's own doc comment).
const maxLogInstances = 64

// logInstances collects the deployment's live instance UUIDs, then - up to
// maxLogInstances - any instance UUID that appears in its event timeline
// within the requested window. store.DeleteInstance removes the Instance
// row once the agent confirms teardown (see internal/server/store/deploy.go),
// so a replica stopped moments ago has already vanished from Inventory by
// the time this runs; its UUID survives only in instance_events, which is
// exactly where the UI's "view logs for this deployment" deep link
// (GET /logs/search?instances=..., plan §3.2, §9) still wants it to find
// that replica's last lines.
func (s *Service) logInstances(ctx context.Context, deploymentID uint, live []monitor.InstanceRef, now time.Time, rng time.Duration) ([]string, error) {
	seen := make(map[string]bool, len(live))
	out := make([]string, 0, len(live))
	add := func(uuid string) bool {
		if uuid == "" || seen[uuid] {
			return true
		}
		seen[uuid] = true
		out = append(out, uuid)
		return len(out) < maxLogInstances
	}
	for _, inst := range live {
		if !add(inst.UUID) {
			return out, nil
		}
	}
	if s.deps.DeploymentEvents == nil {
		return out, nil
	}
	sinceMS := now.Add(-rng).UnixMilli()
	evs, err := s.deploymentEventsInRange(ctx, deploymentID, sinceMS)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		if !add(e.InstanceUUID) {
			break
		}
	}
	return out, nil
}

// buildAlignment assembles the service-layer proxy/engine request-count
// cross-check (plan §3.2: "proxy 请求数 / 引擎完成数 / 客户端错误...不一致时
// 写'请求在 proxy 与引擎之间丢失 N'"), over the same DiagnosisWindow span
// Window's own proxy-derived fields already cover:
//
//   - ProxyRequests, ClientErrors (4xx + 429) and ServerErrors (5xx +
//     asleep_503) come straight from Window's already-computed
//     ProxyRequestsDelta/ClientErrorsDelta/ServerErrorsDelta - the exact
//     counter growth over the window (first vs last sample, walked
//     point-to-point and reset-aware), not RequestsPerSec/ErrRate*'s
//     rate-then-multiply-by-span approximation - no extra query, since
//     Window already reduced the proxy counters for the diagnosis rules.
//     See dto.MonitorAlignment's doc comment for why 4xx and 429 are the
//     client's fault while 5xx and asleep_503 are the server's.
//   - InFlight is Window's InflightLast: the proxy's in-flight gauge
//     sampled at the end of the same window - a displayed figure, not a
//     term subtracted from Lost (see Lost's own note below for why).
//   - EngineCompleted is the one genuinely new query this row needs: the
//     engine's own gpuworks_infer_requests_success_total counter, reduced
//     with the identical reset-aware delta (monitor.SumSeriesDelta) so it
//     is directly comparable to ProxyRequests instead of a smoothed rate.
//   - Lost is proxy_requests - engine_completed - client_errors -
//     server_errors, floored at 0. InFlight is deliberately NOT subtracted
//     here: proxy/accounting.go only increments gpuworks_proxy_requests_total
//     at Finish or RecordImmediate, i.e. once a request has already
//     resolved to one of its classes - a request still in flight has not
//     yet been counted in ProxyRequests (or any of the other three) at
//     all, so there is nothing to net out of an equation it was never part
//     of. Subtracting it would double-discount a gap InFlight cannot
//     actually explain.
//   - DiscrepancyPersistent instead uses InFlight as an upper bound on how
//     much of Lost a request-still-streaming explanation could possibly
//     cover: true only when the in-flight gauge was collected at all and
//     Lost exceeds the largest in-flight count observed anywhere in the
//     window (Window.InflightMax) - i.e. even crediting every concurrently
//     open request at its peak to Lost, the gap still would not close, so
//     it cannot be dismissed as "some of those are just still streaming".
//     Without an in-flight series at all, DiscrepancyPersistent is false
//     (nothing to compare Lost against).
//
// nil (not a zero-valued struct) until the deployment has any proxy traffic
// at all, or while its engine reports no completion counter, so the frontend
// knows to hide the row rather than render zeros or a loss it cannot know.
func (s *Service) buildAlignment(ctx context.Context, id uint, instances []monitor.InstanceRef, now time.Time, w monitor.Window) (*dto.MonitorAlignment, error) {
	if !w.HasRequestRate {
		return nil, nil
	}
	proxyRequests := int64(math.Round(w.ProxyRequestsDelta))
	clientErrors := int64(math.Round(w.ClientErrorsDelta))
	serverErrors := int64(math.Round(w.ServerErrorsDelta))
	inFlight := int64(math.Round(w.InflightLast))

	engineCompleted := int64(0)
	if s.deps.TSDB != nil {
		startMS := now.Add(-w.Span).UnixMilli()
		endMS := now.UnixMilli()
		stepMS := monitor.WindowStep.Milliseconds()
		matcher := map[string]string{"deployment": strconv.FormatUint(uint64(id), 10)}
		success, err := s.deps.TSDB.QueryRange("gpuworks_infer_requests_success_total", matcher, startMS, endMS, stepMS)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			if inst.UUID == "" {
				continue
			}
			covered := false
			for _, ser := range success {
				if ser.Labels["instance"] == inst.UUID {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
			instSeries, err := s.deps.TSDB.QueryRange("gpuworks_infer_requests_success_total", map[string]string{"instance": inst.UUID}, startMS, endMS, stepMS)
			if err != nil {
				return nil, err
			}
			success = append(success, instSeries...)
		}
		// An engine that reports no completions at all (SGLang without
		// --enable-metrics, an engine with no /metrics) leaves nothing to
		// reconcile the proxy against: its zero is unknown, not a loss.
		if len(success) == 0 {
			return nil, nil
		}
		engineCompleted = int64(math.Round(monitor.SumSeriesDelta(success)))
	}

	lost := proxyRequests - engineCompleted - clientErrors - serverErrors
	if lost < 0 {
		lost = 0
	}
	discrepancyPersistent := w.HasInflight && lost > int64(math.Round(w.InflightMax))
	return &dto.MonitorAlignment{
		ProxyRequests: proxyRequests, EngineCompleted: engineCompleted,
		ClientErrors: clientErrors, ServerErrors: serverErrors,
		InFlight: inFlight, Lost: lost, DiscrepancyPersistent: discrepancyPersistent,
	}, nil
}

// ttftBreakdownDTO splits TTFT p95 into queue/prefill/first-token
// components (plan §3.2), straight from Window's already-computed
// quantile-gauge fields - no extra query. nil until TTFT is collected.
//
// QueueMs, PrefillMs and TotalMs are each an independent p95 (see
// dto.MonitorTTFTBreakdown's doc comment), so queue+prefill is not
// guaranteed to fit inside TTFT's own p95: when it doesn't, both are scaled
// down proportionally so the three segments actually sum to TotalMs (a bar
// chart segment that overflows its own total bar would be a worse lie than
// a labeled approximation), and Approximate is set. FirstTokenMs is
// TotalMs minus the (possibly scaled) queue+prefill, clamped to 0 for the
// same reason - a floating-point remainder after scaling could otherwise
// leak a tiny negative value - which also sets Approximate.
func ttftBreakdownDTO(w monitor.Window) *dto.MonitorTTFTBreakdown {
	if !w.HasTTFT {
		return nil
	}
	totalMs := w.TTFTP95Sec * 1000
	queueMs, prefillMs := 0.0, 0.0
	if w.HasQueueTime {
		queueMs = w.QueueTimeP95Sec * 1000
	}
	if w.HasPrefillTime {
		prefillMs = w.PrefillTimeP95Sec * 1000
	}

	approximate := false
	if sum := queueMs + prefillMs; sum > totalMs && sum > 0 {
		scale := totalMs / sum
		queueMs *= scale
		prefillMs *= scale
		approximate = true
	}
	firstTokenMs := totalMs - queueMs - prefillMs
	if firstTokenMs < 0 {
		firstTokenMs = 0
		approximate = true
	}
	return &dto.MonitorTTFTBreakdown{
		QueueMs: roundMsSnapZero(queueMs), PrefillMs: roundMsSnapZero(prefillMs), FirstTokenMs: roundMsSnapZero(firstTokenMs),
		TotalMs: round1(totalMs), Approximate: approximate,
	}
}

// roundMsSnapZero rounds a millisecond value to one decimal place, first
// snapping anything within 0.05ms of zero to exactly 0 - the proportional
// scaling above (queueMs *= scale, and the totalMs-queueMs-prefillMs
// subtraction for firstTokenMs) can otherwise leave float64 dust (an
// FirstTokenMs of 1.1e-13 instead of exactly 0) that would render on the
// wire as a nonzero, oddly-precise number instead of the clean 0 the math
// actually intends.
func roundMsSnapZero(v float64) float64 {
	if v > -0.05 && v < 0.05 {
		return 0
	}
	return math.Round(v*10) / 10
}

// sourcesDTO is the signal sources on the wire: always an object, never null.
func sourcesDTO(sources map[monitor.Signal]monitor.Source) map[string]string {
	out := make(map[string]string, len(sources))
	for signal, source := range sources {
		out[string(signal)] = string(source)
	}
	return out
}

// chartsDTO converts every chart group. kv values are fractions (0..1) and
// latency/tpot/stage_time values are seconds in monitor.Window/ChartSeries; the
// wire contract wants usage_pct/hit_rate_pct as percentages and
// latency/tpot/stage_time in milliseconds, so those groups are scaled here
// - every other group's values already are the right unit as-is.
func chartsDTO(charts map[string][]monitor.ChartSeries) dto.MonitorCharts {
	return dto.MonitorCharts{
		Latency:     chartSeriesDTO(charts["latency"], 1000),
		TPOT:        chartSeriesDTO(charts["tpot"], 1000),
		Concurrency: chartSeriesDTO(charts["concurrency"], 1),
		KV:          chartSeriesDTO(charts["kv"], 100),
		Throughput:  chartSeriesDTO(charts["throughput"], 1),
		StageTime:   chartSeriesDTO(charts["stage_time"], 1000),
		Preemptions: chartSeriesDTO(charts["preemptions"], 1),
	}
}

// chartSeriesDTO tags each named chart line with a "metric" label carrying
// the wire contract's exact name (chartMetricRename), scales every point by
// scale, and propagates any other label the underlying series already
// carries (concurrency's per-instance "instance" label - see
// monitor/overview_service.go's BuildService - passes through this way).
func chartSeriesDTO(series []monitor.ChartSeries, scale float64) []dto.MonitorSeries {
	out := make([]dto.MonitorSeries, 0, len(series))
	for _, s := range series {
		name := s.Name
		if renamed, ok := chartMetricRename[name]; ok {
			name = renamed
		}
		labels := map[string]string{"metric": name}
		for k, v := range s.Labels {
			labels[k] = v
		}
		out = append(out, dto.MonitorSeries{Labels: labels, Points: scaledTSDBPoints(s.Points, scale)})
	}
	return out
}

// scaledTSDBPoints converts points to wire points, multiplying each value
// by scale (1 for every chart group except kv, which stores 0..1 fractions
// internally where the wire wants a percentage).
func scaledTSDBPoints(points []tsdb.Point, scale float64) []dto.Point {
	out := make([]dto.Point, 0, len(points))
	for _, p := range points {
		out = append(out, dto.Point{float64(p.TimeMS), p.Value * scale})
	}
	return out
}

func replicaRowsDTO(rows []monitor.ReplicaRow, instanceState map[string]string) []dto.MonitorReplicaRow {
	out := make([]dto.MonitorReplicaRow, 0, len(rows))
	for _, row := range rows {
		state := instanceState[row.InstanceUUID]
		if state == "" {
			state = row.State
		}
		gpus := make([]dto.MonitorReplicaGPU, 0, len(row.GPUs))
		indexes := make([]int, 0, len(row.GPUs))
		for _, g := range row.GPUs {
			indexes = append(indexes, g.Index)
			gpus = append(gpus, dto.MonitorReplicaGPU{
				Index: g.Index, UtilPct: collectedFloatPtr(g.HasUtil, round1(g.UtilPct)), TemperatureC: collectedFloatPtr(g.HasTemp, round1(g.TempC)),
				MemBWPct: collectedFloatPtr(g.HasMemBW, round1(g.MemBWPct)),
				PowerW:   collectedFloatPtr(g.HasPower, round1(g.PowerW)),
			})
		}
		out = append(out, dto.MonitorReplicaRow{
			InstanceUUID: row.InstanceUUID, Role: row.Role, State: state, NodeName: row.NodeName, GPUIndexes: indexes,
			Running: collectedFloatPtr(row.HasRunning, round1(row.Running)), Waiting: collectedFloatPtr(row.HasWaiting, round1(row.Waiting)),
			KVUsagePct: collectedFloatPtr(row.HasKVUsage, round1(row.KVUsage*100)), KVHitRatePct: collectedFloatPtr(row.HasKVHitRate, round1(row.KVHitRate*100)),
			PromptTokS: collectedFloatPtr(row.HasTokens, round1(row.PromptTokensPerSec)), GenerationTokS: collectedFloatPtr(row.HasTokens, round1(row.GenTokensPerSec)),
			GPUs: gpus,
		})
	}
	return out
}

// withPrefillStageP95 adds the prefill stage's p95 over the PD window to a PD
// summary, from the request store's latency histograms (the gateway's own
// prefill stage counter has a mean only). A store that cannot answer leaves
// it null.
func (s *Service) withPrefillStageP95(ctx context.Context, id uint, now time.Time, pd *dto.MonitorPD) *dto.MonitorPD {
	if pd == nil || s.deps.Requests == nil {
		return pd
	}
	cells, err := s.deps.Requests.Grid(ctx, id, uxrecord.GridSegments, now.Add(-monitor.DiagnosisWindow).UnixMilli(), now.UnixMilli())
	if err != nil {
		return pd
	}
	var row []uxrecord.Cell
	for _, c := range cells {
		if c.X == uxrecord.LatencyRow(string(uxrecord.SegmentPDPrefill)) {
			row = append(row, c)
		}
	}
	if count, _, p95 := percentiles(row); count > 0 {
		v := round1(p95)
		pd.PrefillStageP95Ms = &v
	}
	return pd
}
