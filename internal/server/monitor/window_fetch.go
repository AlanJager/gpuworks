package monitor

import (
	"strconv"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// WindowSeries is every raw tsdb series ComputeWindow needs for one
// deployment, already scoped to it: FetchWindowSeries fetches it directly
// (one deployment, live queries) for BuildService's single-deployment page;
// GroupWindowSeries derives it in pure Go from a fleet-wide
// FetchFleetWindowSeries result for BuildCluster, so a cluster snapshot
// queries tsdb once per metric total, not once per metric per deployment
// (plan §8 second phase capacity requirement).
type WindowSeries struct {
	Running, Waiting                      []tsdb.Series
	KVUsage, KVQueries, KVHits, KVInstant []tsdb.Series
	PromptTokens, GenTokens               []tsdb.Series

	// TTFT/TPOT/E2E/QueueTime/PrefillTime/DecodeTime each carry every
	// quantile the ingest-time histogram->gauge conversion writes (labeled
	// "quantile": "0.5"/"0.95"/"0.99" - see the phase2 metrics contract's
	// "Quantile-at-ingest" section), already deployment/instance scoped;
	// window_infer.go's filterQuantile splits them out.
	TTFT, TPOT, E2E, QueueTime, PrefillTime, DecodeTime []tsdb.Series
	Preemptions                                         []tsdb.Series

	// ProxyRequestsTotal carries the "class" label (2xx/4xx/429/5xx/
	// asleep_503); ProxyDurationSum/Count/Inflight are single series (no
	// extra label beyond deployment) once scoped to this deployment.
	ProxyRequestsTotal                                  []tsdb.Series
	ProxyDurationSum, ProxyDurationCount, ProxyInflight []tsdb.Series

	// Gateway is what stands in for engine series the engine does not
	// report; Sources records, per signal, which one each engine field above
	// now holds (window_sources.go).
	Gateway GatewaySeries
	Sources map[Signal]Source
}

// FleetWindowSeries is the fleet-wide (no matcher) result of querying every
// WindowSeries metric exactly once. BuildCluster fetches this a single time
// per snapshot (FetchFleetWindowSeries) and calls GroupWindowSeries once per
// deployment to slice it up in memory.
type FleetWindowSeries struct {
	Running, Waiting                                    []tsdb.Series
	KVUsage, KVQueries, KVHits, KVInstant               []tsdb.Series
	PromptTokens, GenTokens                             []tsdb.Series
	TTFT, TPOT, E2E, QueueTime, PrefillTime, DecodeTime []tsdb.Series
	Preemptions                                         []tsdb.Series
	ProxyRequestsTotal                                  []tsdb.Series
	ProxyDurationSum, ProxyDurationCount, ProxyInflight []tsdb.Series
	Gateway                                             GatewaySeries
}

// deploymentScopedMetrics is the list of gpuworks_infer_* metric names that
// carry (or, for an old agent, can fall back to) per-instance labels and are
// matched by "deployment" first, "instance" second - every field in
// WindowSeries/FleetWindowSeries above except the GPU-, node- and
// proxy-scoped ones. Declared once so FetchFleetWindowSeries and
// FetchWindowSeries both iterate the identical metric list instead of two
// hand-written call sequences drifting apart.
var deploymentScopedMetrics = []string{
	"gpuworks_infer_requests_running",
	"gpuworks_infer_requests_waiting",
	"gpuworks_infer_gpu_cache_usage_percent",
	"gpuworks_infer_kv_queries_total",
	"gpuworks_infer_kv_hits_total",
	"gpuworks_infer_kv_hit_rate_instant",
	"gpuworks_infer_prompt_tokens_total",
	"gpuworks_infer_generation_tokens_total",
	"gpuworks_infer_ttft_seconds",
	"gpuworks_infer_tpot_seconds",
	"gpuworks_infer_e2e_seconds",
	"gpuworks_infer_queue_time_seconds",
	"gpuworks_infer_prefill_time_seconds",
	"gpuworks_infer_decode_time_seconds",
	"gpuworks_infer_preemptions_total",
}

const (
	metricGPUMemBW = "gpuworks_gpu_memory_bandwidth_utilization"

	metricProxyRequestsTotal = "gpuworks_proxy_requests_total"
	metricProxyDurationSum   = "gpuworks_proxy_request_duration_ms_sum"
	metricProxyDurationCount = "gpuworks_proxy_request_duration_ms_count"
	metricProxyInflight      = "gpuworks_proxy_requests_inflight"
)

// depKeyString formats a deployment id as the string label value tsdb series
// carry.
func depKeyString(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// instanceLabelSet is the set of "instance" label values present in series.
func instanceLabelSet(series []tsdb.Series) map[string]bool {
	set := make(map[string]bool, len(series))
	for _, s := range series {
		if id := s.Labels["instance"]; id != "" {
			set[id] = true
		}
	}
	return set
}

// uncoveredInstances reports which of instances byDeployment (a metric's
// deployment-matched query result) did not attach the "deployment" label
// for - an old agent that predates the "deployment" label never attaches
// it, so its samples only carry "instance". See queryDeploymentMetric's doc
// comment for why this is normally derived once from
// gpuworks_infer_requests_running and reused for every other metric rather
// than recomputed per metric.
func uncoveredInstances(byDeployment []tsdb.Series, instances []InstanceRef) []InstanceRef {
	covered := instanceLabelSet(byDeployment)
	var out []InstanceRef
	for _, inst := range instances {
		if inst.UUID != "" && !covered[inst.UUID] {
			out = append(out, inst)
		}
	}
	return out
}

// filterByLabel returns the subset of series whose label value exactly
// matches value.
func filterByLabel(series []tsdb.Series, label, value string) []tsdb.Series {
	var out []tsdb.Series
	for _, s := range series {
		if s.Labels[label] == value {
			out = append(out, s)
		}
	}
	return out
}

// ---- single-deployment fetch (BuildService) ----

// queryRunningWithCoverage queries gpuworks_infer_requests_running with the
// deployment-then-per-instance-fallback merge queryDeploymentMetric applies,
// and also returns which instances the deployment-matched half of that
// query did not cover - callers reuse this single result across every other
// metric instead of asking queryDeploymentMetric to re-derive it per metric.
func queryRunningWithCoverage(store tsdb.Store, deploymentKey string, instances []InstanceRef, startMS, endMS, stepMS int64) (series []tsdb.Series, uncovered []InstanceRef, err error) {
	const metric = "gpuworks_infer_requests_running"
	byDeployment, err := store.QueryRange(metric, map[string]string{"deployment": deploymentKey}, startMS, endMS, stepMS)
	if err != nil {
		return nil, nil, err
	}
	uncovered = uncoveredInstances(byDeployment, instances)
	series = byDeployment
	for _, inst := range uncovered {
		instSeries, err := store.QueryRange(metric, map[string]string{"instance": inst.UUID}, startMS, endMS, stepMS)
		if err != nil {
			return nil, nil, err
		}
		series = append(series, instSeries...)
	}
	return series, uncovered, nil
}

// queryDeploymentMetric queries metric by the "deployment" label, then fills
// in whichever of uncovered are not already present in that same result by
// querying them directly by their "instance" label and merging. uncovered is
// normally the value queryRunningWithCoverage returned for this deployment
// and time range - but this merge does not simply trust that list: it also
// checks metric's own deployment-matched result for each candidate
// instance's label first, and skips the fallback query entirely when that
// instance is already present, so double counting is impossible no matter
// how uncovered was derived (including a Callable/custom engine that maps
// this metric but never emits requests_running at all).
func queryDeploymentMetric(store tsdb.Store, metric, deploymentKey string, uncovered []InstanceRef, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	series, err := store.QueryRange(metric, map[string]string{"deployment": deploymentKey}, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	coveredHere := instanceLabelSet(series)
	for _, inst := range uncovered {
		if inst.UUID == "" || coveredHere[inst.UUID] {
			continue
		}
		instSeries, err := store.QueryRange(metric, map[string]string{"instance": inst.UUID}, startMS, endMS, stepMS)
		if err != nil {
			return nil, err
		}
		series = append(series, instSeries...)
	}
	return series, nil
}

func queryProxyMetrics(store tsdb.Store, deploymentKey string, startMS, endMS, stepMS int64) (requestsTotal, durationSum, durationCount, inflight []tsdb.Series, err error) {
	matcher := map[string]string{"deployment": deploymentKey}
	if requestsTotal, err = store.QueryRange(metricProxyRequestsTotal, matcher, startMS, endMS, stepMS); err != nil {
		return
	}
	if durationSum, err = store.QueryRange(metricProxyDurationSum, matcher, startMS, endMS, stepMS); err != nil {
		return
	}
	if durationCount, err = store.QueryRange(metricProxyDurationCount, matcher, startMS, endMS, stepMS); err != nil {
		return
	}
	inflight, err = store.QueryRange(metricProxyInflight, matcher, startMS, endMS, stepMS)
	return
}

// FetchWindowSeries queries tsdb for exactly one deployment's WindowSeries
// (BuildService's single-deployment page - fetching the whole fleet just
// for one deployment's page would be wasteful in the other direction). It
// never populates Idle; callers that need it call FetchIdleSeries
// separately once they know (needsIdleCheck) it is actually warranted.
func FetchWindowSeries(store tsdb.Store, deploymentID uint, instances []InstanceRef, now, spanStart time.Time, stepMS int64) (WindowSeries, error) {
	depKey := depKeyString(deploymentID)
	startMS := spanStart.UnixMilli()
	endMS := now.UnixMilli()

	running, uncovered, err := queryRunningWithCoverage(store, depKey, instances, startMS, endMS, stepMS)
	if err != nil {
		return WindowSeries{}, err
	}
	query := func(metric string) ([]tsdb.Series, error) {
		return queryDeploymentMetric(store, metric, depKey, uncovered, startMS, endMS, stepMS)
	}

	ws := WindowSeries{Running: running}
	fields := map[string]*[]tsdb.Series{
		"gpuworks_infer_requests_waiting":        &ws.Waiting,
		"gpuworks_infer_gpu_cache_usage_percent": &ws.KVUsage,
		"gpuworks_infer_kv_queries_total":        &ws.KVQueries,
		"gpuworks_infer_kv_hits_total":           &ws.KVHits,
		"gpuworks_infer_kv_hit_rate_instant":     &ws.KVInstant,
		"gpuworks_infer_prompt_tokens_total":     &ws.PromptTokens,
		"gpuworks_infer_generation_tokens_total": &ws.GenTokens,
		"gpuworks_infer_ttft_seconds":            &ws.TTFT,
		"gpuworks_infer_tpot_seconds":            &ws.TPOT,
		"gpuworks_infer_e2e_seconds":             &ws.E2E,
		"gpuworks_infer_queue_time_seconds":      &ws.QueueTime,
		"gpuworks_infer_prefill_time_seconds":    &ws.PrefillTime,
		"gpuworks_infer_decode_time_seconds":     &ws.DecodeTime,
		"gpuworks_infer_preemptions_total":       &ws.Preemptions,
	}
	for _, metric := range deploymentScopedMetrics {
		dst, ok := fields[metric]
		if !ok {
			continue // gpuworks_infer_requests_running, already fetched above
		}
		s, err := query(metric)
		if err != nil {
			return WindowSeries{}, err
		}
		*dst = s
	}

	if ws.ProxyRequestsTotal, ws.ProxyDurationSum, ws.ProxyDurationCount, ws.ProxyInflight, err =
		queryProxyMetrics(store, depKey, startMS, endMS, stepMS); err != nil {
		return WindowSeries{}, err
	}
	if ws.Gateway, err = queryGatewaySeries(store, depKey, startMS, endMS, stepMS); err != nil {
		return WindowSeries{}, err
	}
	ws.resolveSources()
	return ws, nil
}

// FetchFleetWindowSeries queries every WindowSeries metric fleet-wide (no
// matcher) exactly once, for BuildCluster to group per deployment in memory
// via GroupWindowSeries.
func FetchFleetWindowSeries(store tsdb.Store, now, spanStart time.Time, stepMS int64) (FleetWindowSeries, error) {
	startMS := spanStart.UnixMilli()
	endMS := now.UnixMilli()
	query := func(metric string) ([]tsdb.Series, error) {
		return store.QueryRange(metric, nil, startMS, endMS, stepMS)
	}

	var fleet FleetWindowSeries
	fields := map[string]*[]tsdb.Series{
		"gpuworks_infer_requests_running":        &fleet.Running,
		"gpuworks_infer_requests_waiting":        &fleet.Waiting,
		"gpuworks_infer_gpu_cache_usage_percent": &fleet.KVUsage,
		"gpuworks_infer_kv_queries_total":        &fleet.KVQueries,
		"gpuworks_infer_kv_hits_total":           &fleet.KVHits,
		"gpuworks_infer_kv_hit_rate_instant":     &fleet.KVInstant,
		"gpuworks_infer_prompt_tokens_total":     &fleet.PromptTokens,
		"gpuworks_infer_generation_tokens_total": &fleet.GenTokens,
		"gpuworks_infer_ttft_seconds":            &fleet.TTFT,
		"gpuworks_infer_tpot_seconds":            &fleet.TPOT,
		"gpuworks_infer_e2e_seconds":             &fleet.E2E,
		"gpuworks_infer_queue_time_seconds":      &fleet.QueueTime,
		"gpuworks_infer_prefill_time_seconds":    &fleet.PrefillTime,
		"gpuworks_infer_decode_time_seconds":     &fleet.DecodeTime,
		"gpuworks_infer_preemptions_total":       &fleet.Preemptions,
		metricProxyRequestsTotal:                 &fleet.ProxyRequestsTotal,
		metricProxyDurationSum:                   &fleet.ProxyDurationSum,
		metricProxyDurationCount:                 &fleet.ProxyDurationCount,
		metricProxyInflight:                      &fleet.ProxyInflight,
	}
	for metric, dst := range fields {
		s, err := query(metric)
		if err != nil {
			return FleetWindowSeries{}, err
		}
		*dst = s
	}
	gateway, err := queryFleetGatewaySeries(store, startMS, endMS, stepMS)
	if err != nil {
		return FleetWindowSeries{}, err
	}
	fleet.Gateway = gateway
	return fleet, nil
}

// indexBuildCount counts every fleetIndex/gpuIndex/nodeIndex/deployment-only
// index construction. It exists purely so tests can prove
// BuildFleetWindowIndex builds every index exactly once per snapshot,
// regardless of the fleet's deployment count (see
// TestBuildFleetWindowIndexIsReusedNotRebuiltPerDeployment) - production
// code never reads it.
var indexBuildCount int64

// fleetIndex indexes one metric's fleet-wide series by "deployment" and
// "instance" label value, so GroupWindowSeries can reproduce
// queryDeploymentMetric's merge semantics over already-fetched series
// instead of issuing a query.
type fleetIndex struct {
	byDeployment map[string][]tsdb.Series
	byInstance   map[string][]tsdb.Series
}

func indexFleetSeries(series []tsdb.Series) fleetIndex {
	atomic.AddInt64(&indexBuildCount, 1)
	idx := fleetIndex{byDeployment: map[string][]tsdb.Series{}, byInstance: map[string][]tsdb.Series{}}
	for _, s := range series {
		if d := s.Labels["deployment"]; d != "" {
			idx.byDeployment[d] = append(idx.byDeployment[d], s)
		}
		if i := s.Labels["instance"]; i != "" {
			idx.byInstance[i] = append(idx.byInstance[i], s)
		}
	}
	return idx
}

// forDeployment mirrors queryDeploymentMetric's merge: the deployment-
// matched series plus, for each of uncovered not already present in that
// same result, its per-instance series.
func (idx fleetIndex) forDeployment(depKey string, uncovered []InstanceRef) []tsdb.Series {
	matched := idx.byDeployment[depKey]
	coveredHere := instanceLabelSet(matched)
	out := append([]tsdb.Series(nil), matched...)
	for _, inst := range uncovered {
		if inst.UUID == "" || coveredHere[inst.UUID] {
			continue
		}
		out = append(out, idx.byInstance[inst.UUID]...)
	}
	return out
}

// deploymentIndex indexes fleet-wide series by their plain "deployment"
// label, with no instance fallback - the proxy metrics' own shape (always
// deployment-labeled, generated by the server itself; see
// window_proxy.go's doc comment).
type deploymentIndex map[string][]tsdb.Series

func indexByDeployment(series []tsdb.Series) deploymentIndex {
	atomic.AddInt64(&indexBuildCount, 1)
	idx := deploymentIndex{}
	for _, s := range series {
		if d := s.Labels["deployment"]; d != "" {
			idx[d] = append(idx[d], s)
		}
	}
	return idx
}

// FleetWindowIndex is FleetWindowSeries pre-indexed once per snapshot: every
// metric's fleetIndex/gpuIndex/nodeIndex/deploymentIndex is built exactly
// once here (BuildFleetWindowIndex), and GroupWindowSeries below does only
// O(1) map lookups (plus the small, per-deployment uncovered-instance
// merge) per deployment - not a fresh O(fleet series) scan per deployment
// per metric, which is what re-indexing inside GroupWindowSeries itself
// used to cost for every deployment in the fleet.
type FleetWindowIndex struct {
	running, waiting                                    fleetIndex
	kvUsage, kvQueries, kvHits, kvInstant               fleetIndex
	promptTokens, genTokens                             fleetIndex
	ttft, tpot, e2e, queueTime, prefillTime, decodeTime fleetIndex
	preemptions                                         fleetIndex

	proxyRequestsTotal, proxyDurationSum, proxyDurationCount, proxyInflight deploymentIndex
	gateway                                                                 gatewayIndex
}

// BuildFleetWindowIndex indexes every FleetWindowSeries field once. Callers
// (BuildCluster) build this a single time per snapshot and pass it to
// GroupWindowSeries once per deployment.
func BuildFleetWindowIndex(fleet FleetWindowSeries) FleetWindowIndex {
	return FleetWindowIndex{
		running:      indexFleetSeries(fleet.Running),
		waiting:      indexFleetSeries(fleet.Waiting),
		kvUsage:      indexFleetSeries(fleet.KVUsage),
		kvQueries:    indexFleetSeries(fleet.KVQueries),
		kvHits:       indexFleetSeries(fleet.KVHits),
		kvInstant:    indexFleetSeries(fleet.KVInstant),
		promptTokens: indexFleetSeries(fleet.PromptTokens),
		genTokens:    indexFleetSeries(fleet.GenTokens),
		ttft:         indexFleetSeries(fleet.TTFT),
		tpot:         indexFleetSeries(fleet.TPOT),
		e2e:          indexFleetSeries(fleet.E2E),
		queueTime:    indexFleetSeries(fleet.QueueTime),
		prefillTime:  indexFleetSeries(fleet.PrefillTime),
		decodeTime:   indexFleetSeries(fleet.DecodeTime),
		preemptions:  indexFleetSeries(fleet.Preemptions),

		proxyRequestsTotal: indexByDeployment(fleet.ProxyRequestsTotal),
		proxyDurationSum:   indexByDeployment(fleet.ProxyDurationSum),
		proxyDurationCount: indexByDeployment(fleet.ProxyDurationCount),
		proxyInflight:      indexByDeployment(fleet.ProxyInflight),
		gateway:            indexGatewaySeries(fleet.Gateway),
	}
}

// GroupWindowSeries derives one deployment's WindowSeries from a
// pre-built FleetWindowIndex, purely in memory - no tsdb query and no index
// (re)construction. This is what lets BuildCluster fetch every metric
// exactly once per snapshot (FetchFleetWindowSeries), index it exactly once
// (BuildFleetWindowIndex), and still get per-deployment ComputeWindow
// inputs for every deployment in the fleet.
func GroupWindowSeries(idx FleetWindowIndex, deploymentID uint, instances []InstanceRef) WindowSeries {
	depKey := depKeyString(deploymentID)
	uncovered := uncoveredInstances(idx.running.byDeployment[depKey], instances)
	dep := func(fi fleetIndex) []tsdb.Series { return fi.forDeployment(depKey, uncovered) }

	ws := WindowSeries{
		Running:      idx.running.forDeployment(depKey, uncovered),
		Waiting:      dep(idx.waiting),
		KVUsage:      dep(idx.kvUsage),
		KVQueries:    dep(idx.kvQueries),
		KVHits:       dep(idx.kvHits),
		KVInstant:    dep(idx.kvInstant),
		PromptTokens: dep(idx.promptTokens),
		GenTokens:    dep(idx.genTokens),
		TTFT:         dep(idx.ttft),
		TPOT:         dep(idx.tpot),
		E2E:          dep(idx.e2e),
		QueueTime:    dep(idx.queueTime),
		PrefillTime:  dep(idx.prefillTime),
		DecodeTime:   dep(idx.decodeTime),
		Preemptions:  dep(idx.preemptions),

		ProxyRequestsTotal: idx.proxyRequestsTotal[depKey],
		ProxyDurationSum:   idx.proxyDurationSum[depKey],
		ProxyDurationCount: idx.proxyDurationCount[depKey],
		ProxyInflight:      idx.proxyInflight[depKey],
		Gateway:            idx.gateway[depKey],
	}
	ws.resolveSources()
	return ws
}
