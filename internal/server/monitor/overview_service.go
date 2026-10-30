package monitor

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// ServiceSnapshot is the /monitor/d/:id service-layer view (plan §3.2).
type ServiceSnapshot struct {
	AsOf         time.Time
	Stale        bool
	DeploymentID uint

	// Window is the DiagnosisWindow-scoped Window, carried through so
	// monitorapi can build the alignment row's TTFT breakdown
	// (queue/prefill/first-token/total ms) straight from already-computed
	// fields instead of a second ComputeWindow call.
	Window Window

	// Charts is keyed by chart id: "latency" (ttft/tpot/e2e quantiles),
	// "concurrency" (running/waiting), "kv" (usage + hit rate), "throughput"
	// (prompt/generation tok/s), "stage_time" (queue/prefill/decode p95),
	// "preemptions" (preemptions/s).
	Charts map[string][]ChartSeries
	// Sources says, per signal the gateway can stand in for, which source
	// the charts drew it from; a signal neither provides is absent.
	Sources map[Signal]Source

	Replicas []ReplicaRow
}

// ChartSeries is one named/labeled line of a service-layer chart.
type ChartSeries struct {
	Name   string
	Labels map[string]string
	Points []tsdb.Point
}

// ReplicaRow is one row of the service-layer replica table (plan §3.2).
type ReplicaRow struct {
	InstanceUUID string
	NodeUUID     string
	NodeName     string
	State        string
	// Role is the replica's PD role, empty outside PD deployments.
	Role string

	HasRunning bool
	Running    float64
	HasWaiting bool
	Waiting    float64

	HasKVUsage bool
	KVUsage    float64

	HasKVHitRate bool
	KVHitRate    float64

	HasTokens          bool
	PromptTokensPerSec float64
	GenTokensPerSec    float64

	GPUs []ReplicaGPU
}

// ReplicaGPU is one GPU row within a ReplicaRow.
type ReplicaGPU struct {
	Index int

	HasUtil bool
	UtilPct float64

	HasMemBW bool
	// MemBWPct is a percent (0..100), unlike Window.MemBWUtilAvg's fraction
	// - this row feeds the wire's mem_bw_pct field directly with no further
	// scaling, so it is stored in that same unit here.
	MemBWPct float64

	HasTemp bool
	TempC   float64

	HasPower bool
	// PowerW is the card's average draw over the window, in watts - the
	// metric's own unit (gpuworks_gpu_power_watts, internal/platformmetrics'
	// "watts"), so it needs no scaling here or at the wire boundary.
	PowerW float64
}

// deploymentNotFoundError reports a BuildService call for a deployment id
// the current Inventory does not contain (deleted between the page load and
// the drill-down click, or a bad id typed into the URL).
type deploymentNotFoundError struct{ id uint }

func (e *deploymentNotFoundError) Error() string {
	return fmt.Sprintf("monitor: deployment %d not found", e.id)
}

// BuildService assembles the service-layer snapshot for one deployment and
// chart range.
func BuildService(ctx context.Context, deps Deps, deploymentID uint, rng time.Duration) (ServiceSnapshot, error) {
	if deps.TSDB == nil {
		return ServiceSnapshot{}, fmt.Errorf("monitor: BuildService requires a non-nil Deps.TSDB")
	}
	if err := ctx.Err(); err != nil {
		return ServiceSnapshot{}, err
	}
	now := deps.now()
	inv, err := deps.Inventory(ctx)
	if err != nil {
		return ServiceSnapshot{}, err
	}

	var dep *DeploymentRef
	for i := range inv.Deployments {
		if inv.Deployments[i].ID == deploymentID {
			dep = &inv.Deployments[i]
			break
		}
	}
	if dep == nil {
		return ServiceSnapshot{}, &deploymentNotFoundError{id: deploymentID}
	}

	var instances []InstanceRef
	for _, inst := range inv.Instances {
		if inst.DeploymentID == deploymentID {
			instances = append(instances, inst)
		}
	}
	nodeNames := nodeNameIndex(inv.Nodes)

	series, err := FetchWindowSeries(deps.TSDB, deploymentID, instances, now, now.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		return ServiceSnapshot{}, err
	}
	pd, err := pdSeriesFor(deps.TSDB, *dep, instances, &series, now)
	if err != nil {
		return ServiceSnapshot{}, err
	}
	w := ComputeWindow(series, WindowParams{Now: now, Span: DiagnosisWindow})
	if pd != nil {
		ComputePDWindow(&w, *pd, dep.PDTransferMetrics)
	}

	snap := ServiceSnapshot{
		DeploymentID: deploymentID,
		Window:       w,
		Charts:       map[string][]ChartSeries{},
	}

	charts, sources, err := buildServiceCharts(deps.TSDB, deploymentID, instances, now, rng)
	if err != nil {
		return ServiceSnapshot{}, err
	}
	snap.Sources = sources
	for k, v := range charts {
		snap.Charts[k] = v
	}

	// Headless distributed workers (InstanceRef.Headless) never get their
	// own replica row: they are process-group participants with no API
	// endpoint of their own, not a unit of capacity the replica table
	// presents (see InstanceRef.Headless's doc comment and ReadyReplicas,
	// its cluster-layer sibling).
	for _, inst := range instances {
		if inst.Headless {
			continue
		}
		row, err := buildReplicaRow(deps.TSDB, inst, nodeNames, now)
		if err != nil {
			return ServiceSnapshot{}, err
		}
		snap.Replicas = append(snap.Replicas, row)
	}

	var asOf time.Time
	noteAsOf := func(t time.Time) {
		if !t.IsZero() && (asOf.IsZero() || t.After(asOf)) {
			asOf = t
		}
	}
	noteAsOf(w.LatestSampleAt)
	var chartSeries []tsdb.Series
	for _, series := range snap.Charts {
		chartSeries = append(chartSeries, toSeries(flattenPoints(series))...)
	}
	if p, ok := LatestValue(chartSeries); ok {
		noteAsOf(time.UnixMilli(p.TimeMS).UTC())
	}
	snap.AsOf = asOf
	snap.Stale = asOf.IsZero() || now.Sub(asOf) > staleAfter
	return snap, nil
}

func buildServiceCharts(store tsdb.Store, deploymentID uint, instances []InstanceRef, now time.Time, rng time.Duration) (map[string][]ChartSeries, map[Signal]Source, error) {
	stepMS := ChartStep(rng)
	startMS := now.Add(-rng).UnixMilli()
	endMS := now.UnixMilli()
	depKey := depKeyString(deploymentID)

	running, uncovered, err := queryRunningWithCoverage(store, depKey, instances, startMS, endMS, stepMS)
	if err != nil {
		return nil, nil, err
	}

	query := func(metric string) ([]tsdb.Series, error) {
		return queryDeploymentMetric(store, metric, depKey, uncovered, startMS, endMS, stepMS)
	}

	waiting, err := query("gpuworks_infer_requests_waiting")
	if err != nil {
		return nil, nil, err
	}
	kvUsage, err := query("gpuworks_infer_gpu_cache_usage_percent")
	if err != nil {
		return nil, nil, err
	}
	kvQueries, err := query("gpuworks_infer_kv_queries_total")
	if err != nil {
		return nil, nil, err
	}
	kvHits, err := query("gpuworks_infer_kv_hits_total")
	if err != nil {
		return nil, nil, err
	}
	kvInstant, err := query("gpuworks_infer_kv_hit_rate_instant")
	if err != nil {
		return nil, nil, err
	}
	promptTokens, err := query("gpuworks_infer_prompt_tokens_total")
	if err != nil {
		return nil, nil, err
	}
	genTokens, err := query("gpuworks_infer_generation_tokens_total")
	if err != nil {
		return nil, nil, err
	}
	ttft, err := query("gpuworks_infer_ttft_seconds")
	if err != nil {
		return nil, nil, err
	}
	tpot, err := query("gpuworks_infer_tpot_seconds")
	if err != nil {
		return nil, nil, err
	}
	queueTime, err := query("gpuworks_infer_queue_time_seconds")
	if err != nil {
		return nil, nil, err
	}
	prefillTime, err := query("gpuworks_infer_prefill_time_seconds")
	if err != nil {
		return nil, nil, err
	}
	decodeTime, err := query("gpuworks_infer_decode_time_seconds")
	if err != nil {
		return nil, nil, err
	}
	preemptions, err := query("gpuworks_infer_preemptions_total")
	if err != nil {
		return nil, nil, err
	}
	e2e, err := query("gpuworks_infer_e2e_seconds")
	if err != nil {
		return nil, nil, err
	}
	// The signals the gateway can stand in for are resolved exactly as the
	// window resolves them (window_sources.go), over the chart's range.
	resolved := WindowSeries{
		Running: running, PromptTokens: promptTokens, GenTokens: genTokens, TTFT: ttft, E2E: e2e,
		TPOT: tpot, QueueTime: queueTime, PrefillTime: prefillTime, DecodeTime: decodeTime,
	}
	if resolved.ProxyInflight, err = store.QueryRange(metricProxyInflight, map[string]string{"deployment": depKey}, startMS, endMS, stepMS); err != nil {
		return nil, nil, err
	}
	if resolved.Gateway, err = queryGatewaySeries(store, depKey, startMS, endMS, stepMS); err != nil {
		return nil, nil, err
	}
	resolved.resolveSources()
	running, promptTokens, genTokens, ttft, e2e = resolved.Running, resolved.PromptTokens, resolved.GenTokens, resolved.TTFT, resolved.E2E
	tpot, queueTime, prefillTime, decodeTime = resolved.TPOT, resolved.QueueTime, resolved.PrefillTime, resolved.DecodeTime

	charts := map[string][]ChartSeries{}
	// latency, tpot and stage_time are combined across every replica (MaxBy(...,
	// "") - one line per quantile/stage), unlike concurrency/kv/throughput
	// below: the wire contract's labels.metric for these two never mentions
	// a per-instance "instance" label (plan §6).
	charts["latency"] = namedSeries("ttft_p50", AvgBy(filterQuantile(ttft, quantileP50), ""))
	charts["latency"] = append(charts["latency"], namedSeries("ttft_p95", MaxBy(filterQuantile(ttft, quantileP95), ""))...)
	charts["latency"] = append(charts["latency"], namedSeries("ttft_p99", MaxBy(filterQuantile(ttft, quantileP99), ""))...)
	charts["latency"] = append(charts["latency"], namedSeries("e2e_p95", MaxBy(filterQuantile(e2e, quantileP95), ""))...)
	// TPOT has a chart of its own: its milliseconds sit on another scale than
	// TTFT's seconds, and on one axis they flattened into the floor.
	charts["tpot"] = namedSeries("tpot_p95", MaxBy(filterQuantile(tpot, quantileP95), ""))
	charts["tpot"] = append(charts["tpot"], namedSeries("tpot_p99", MaxBy(filterQuantile(tpot, quantileP99), ""))...)

	// Named "*_ms" per the wire contract even though the values here are
	// still seconds (Window's own unit): chartsDTO (monitorapi) applies the
	// x1000 conversion at the wire boundary, the same place it already
	// converts kv's fraction to a percent.
	charts["stage_time"] = namedSeries("queue_ms", MaxBy(filterQuantile(queueTime, quantileP95), ""))
	charts["stage_time"] = append(charts["stage_time"], namedSeries("prefill_ms", MaxBy(filterQuantile(prefillTime, quantileP95), ""))...)
	charts["stage_time"] = append(charts["stage_time"], namedSeries("decode_ms", MaxBy(filterQuantile(decodeTime, quantileP95), ""))...)

	charts["preemptions"] = namedSeries("preemptions_per_s", SumBy(Rate(preemptions), ""))
	// Every chart here is grouped by instance, not combined into one
	// deployment total: plan §6's contract draws concurrency, kv and
	// throughput per replica ("labels.instance") so a multi-replica
	// deployment's per-replica load, KV pressure and throughput are each
	// visible on their own chart rather than averaged/summed away - exactly
	// the "one replica is the actual bottleneck" case a deployment total
	// would hide. Every input series here is already one series per
	// instance (queryRunningWithCoverage/queryDeploymentMetric never merge
	// across instances), so grouping by "instance" is a relabel, not a real
	// reduction: SumBy/MaxBy/AvgBy(x, "instance") is a no-op reduction over
	// each single-series group whose only effect is to replace that
	// series' full label set with just {instance: uuid}.
	charts["concurrency"] = namedSeries("running", SumBy(running, "instance"))
	charts["concurrency"] = append(charts["concurrency"], namedSeries("waiting", SumBy(waiting, "instance"))...)

	charts["kv"] = namedSeries("usage", MaxBy(kvUsage, "instance"))
	charts["kv"] = append(charts["kv"], namedSeries("hit_rate", kvHitRateByInstance(kvQueries, kvHits, kvInstant))...)

	charts["throughput"] = namedSeries("prompt_tokens_per_sec", SumBy(Rate(promptTokens), "instance"))
	charts["throughput"] = append(charts["throughput"], namedSeries("generation_tokens_per_sec", SumBy(Rate(genTokens), "instance"))...)

	return charts, resolved.Sources, nil
}

func namedSeries(name string, series []tsdb.Series) []ChartSeries {
	out := make([]ChartSeries, 0, len(series))
	for _, s := range series {
		out = append(out, ChartSeries{Name: name, Labels: s.Labels, Points: s.Points})
	}
	return out
}

func flattenPoints(series []ChartSeries) []tsdb.Point {
	var out []tsdb.Point
	for _, s := range series {
		out = append(out, s.Points...)
	}
	return out
}

func buildReplicaRow(store tsdb.Store, inst InstanceRef, nodeNames map[string]string, now time.Time) (ReplicaRow, error) {
	row := ReplicaRow{
		InstanceUUID: inst.UUID,
		NodeUUID:     inst.NodeUUID,
		NodeName:     nodeNameOrUUID(nodeNames, inst.NodeUUID),
		State:        inst.State,
		Role:         inst.Role,
	}
	if store == nil {
		return row, nil
	}

	startMS := now.Add(-DiagnosisWindow).UnixMilli()
	endMS := now.UnixMilli()
	stepMS := WindowStep.Milliseconds()
	matcher := map[string]string{"instance": inst.UUID}
	query := func(metric string) ([]tsdb.Series, error) {
		return store.QueryRange(metric, matcher, startMS, endMS, stepMS)
	}

	running, err := query("gpuworks_infer_requests_running")
	if err != nil {
		return row, err
	}
	if len(running) > 0 {
		if combined := SumBy(running, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasRunning, row.Running = true, Mean(Values(combined[0]))
		}
	}

	waiting, err := query("gpuworks_infer_requests_waiting")
	if err != nil {
		return row, err
	}
	if len(waiting) > 0 {
		if combined := SumBy(waiting, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasWaiting, row.Waiting = true, Mean(Values(combined[0]))
		}
	}

	kvUsage, err := query("gpuworks_infer_gpu_cache_usage_percent")
	if err != nil {
		return row, err
	}
	if len(kvUsage) > 0 {
		if combined := MaxBy(kvUsage, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasKVUsage, row.KVUsage = true, MaxOf(Values(combined[0]))
		}
	}

	kvQueries, err := query("gpuworks_infer_kv_queries_total")
	if err != nil {
		return row, err
	}
	kvHits, err := query("gpuworks_infer_kv_hits_total")
	if err != nil {
		return row, err
	}
	kvInstant, err := query("gpuworks_infer_kv_hit_rate_instant")
	if err != nil {
		return row, err
	}
	var kv Window
	computeKVHitRate(&kv, kvQueries, kvHits, kvInstant)
	row.HasKVHitRate, row.KVHitRate = kv.HasKVHitRate, kv.KVHitRate

	promptTokens, err := query("gpuworks_infer_prompt_tokens_total")
	if err != nil {
		return row, err
	}
	genTokens, err := query("gpuworks_infer_generation_tokens_total")
	if err != nil {
		return row, err
	}
	row.HasTokens = len(promptTokens) > 0 || len(genTokens) > 0
	if combined := SumBy(Rate(promptTokens), ""); len(combined) > 0 && len(combined[0].Points) > 0 {
		row.PromptTokensPerSec = Mean(Values(combined[0]))
	}
	if combined := SumBy(Rate(genTokens), ""); len(combined) > 0 && len(combined[0].Points) > 0 {
		row.GenTokensPerSec = Mean(Values(combined[0]))
	}

	for _, idx := range inst.GPUIndexes {
		gpu := ReplicaGPU{Index: idx}
		gpuMatcher := map[string]string{"node": inst.NodeUUID, "gpu": fmt.Sprintf("%d", idx)}
		if util, err := store.QueryRange("gpuworks_gpu_utilization", gpuMatcher, startMS, endMS, stepMS); err != nil {
			return row, err
		} else if len(util) > 0 {
			if combined := AvgBy(util, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
				gpu.HasUtil, gpu.UtilPct = true, Mean(Values(combined[0]))
			}
		}
		if memBW, err := store.QueryRange(metricGPUMemBW, gpuMatcher, startMS, endMS, stepMS); err != nil {
			return row, err
		} else if len(memBW) > 0 {
			if combined := AvgBy(memBW, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
				gpu.HasMemBW, gpu.MemBWPct = true, Mean(Values(combined[0]))
			}
		}
		if temp, err := store.QueryRange("gpuworks_gpu_temperature_celsius", gpuMatcher, startMS, endMS, stepMS); err != nil {
			return row, err
		} else if len(temp) > 0 {
			if combined := MaxBy(temp, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
				gpu.HasTemp, gpu.TempC = true, MaxOf(Values(combined[0]))
			}
		}
		// Power takes the mean, not temperature's max: watts is an
		// instantaneous draw that swings with every idle gap and boost burst
		// inside the diagnosis window, so the window's peak reports the one
		// busiest instant rather than what the card is drawing. A missing
		// series (a node whose collector or card does not export the metric)
		// leaves HasPower false, so the wire carries null rather than a
		// fabricated 0 W.
		if power, err := store.QueryRange("gpuworks_gpu_power_watts", gpuMatcher, startMS, endMS, stepMS); err != nil {
			return row, err
		} else if len(power) > 0 {
			if combined := AvgBy(power, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
				gpu.HasPower, gpu.PowerW = true, Mean(Values(combined[0]))
			}
		}
		row.GPUs = append(row.GPUs, gpu)
	}

	return row, nil
}

func nodeNameOrUUID(nodeNames map[string]string, uuid string) string {
	if name, ok := nodeNames[uuid]; ok && name != "" {
		return name
	}
	return uuid
}
