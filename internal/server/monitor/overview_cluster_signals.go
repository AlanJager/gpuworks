package monitor

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file holds BuildCluster's fleet-wide reduction helpers: the
// per-snapshot signal sparklines (clusterWaitingP95 and its
// clusterErrorRateSeries/clusterProxyRequestRate/clusterPreemptionsRate/
// clusterQuantileP95Ms siblings, all reduced from the already-fetched fleet
// series - zero additional tsdb queries), the VRAM three-segment bar
// (vramByNode) and the cluster-wide generation-token chart series
// (clusterTokenRate). Split out of overview_cluster.go to stay under the
// 600-line file-size budget.

// clusterWaitingP95 computes the true cluster-wide queue depth p95 from the
// fleet-wide waiting series FetchFleetWindowSeries already fetched (no
// query of its own - reusing it here instead of a second, identical
// gpuworks_infer_requests_waiting query is exactly what keeps BuildCluster
// at one query per metric per snapshot): every deployment's waiting-requests
// series, summed per bucket into one combined cluster-wide series (not an
// average or max of each deployment's own p95, which would understate a
// cluster where several moderately-queued services add up to a real
// backlog), then the 95th percentile of that series over the window.
func clusterWaitingP95(waiting []tsdb.Series) (series []tsdb.Point, p95 float64, has bool) {
	if len(waiting) == 0 {
		return nil, 0, false
	}
	combined := SumBy(waiting, "")
	if len(combined) == 0 || len(combined[0].Points) == 0 {
		return nil, 0, false
	}
	return combined[0].Points, Quantile(Values(combined[0]), 0.95), true
}

// clusterProxyRequestRate is the cluster-wide combined proxy request rate
// (every deployment, every class summed) - SumBy(Rate(x), "") merges every
// input series into one regardless of its deployment/class labels.
func clusterProxyRequestRate(proxyRequestsTotal []tsdb.Series) []tsdb.Point {
	combined := SumBy(Rate(proxyRequestsTotal), "")
	if len(combined) == 0 {
		return nil
	}
	return combined[0].Points
}

// clusterErrorRateSeries is the cluster-wide combined total-error-rate
// percent (5xx+429+asleep_503 share of all proxy traffic) at each bucket -
// the true fleet-wide ratio, not an average of each deployment's own rate
// (the same "combine first, reduce second" discipline clusterWaitingP95
// applies). It groups every deployment's requests_total series by class
// (ignoring which deployment they came from), sums each class's rate across
// the whole fleet, then divides the combined error classes by the combined
// total.
func clusterErrorRateSeries(proxyRequestsTotal []tsdb.Series) []tsdb.Point {
	byClass := map[string][]tsdb.Series{}
	for _, s := range proxyRequestsTotal {
		byClass[s.Labels["class"]] = append(byClass[s.Labels["class"]], s)
	}
	rateByClass := map[string]tsdb.Series{}
	var totalRates []tsdb.Series
	for class, ss := range byClass {
		summed := SumBy(Rate(ss), "")
		if len(summed) == 0 {
			continue
		}
		rateByClass[class] = summed[0]
		totalRates = append(totalRates, summed[0])
	}
	totalCombined := SumBy(totalRates, "")
	if len(totalCombined) == 0 || len(totalCombined[0].Points) == 0 {
		return nil
	}
	var errRates []tsdb.Series
	for _, class := range []string{errClass5xx, errClass429, errClassAsleep} {
		if r, ok := rateByClass[class]; ok {
			errRates = append(errRates, r)
		}
	}
	if len(errRates) == 0 {
		return nil
	}
	errCombined := SumBy(errRates, "")
	if len(errCombined) == 0 {
		return nil
	}
	ratio := ratioSeries(errCombined[0], totalCombined[0])
	out := make([]tsdb.Point, len(ratio.Points))
	for i, p := range ratio.Points {
		out[i] = tsdb.Point{TimeMS: p.TimeMS, Value: p.Value * 100}
	}
	return out
}

// clusterPreemptionsRate is the cluster-wide combined preemption rate.
func clusterPreemptionsRate(preemptions []tsdb.Series) []tsdb.Point {
	combined := SumBy(Rate(preemptions), "")
	if len(combined) == 0 {
		return nil
	}
	return combined[0].Points
}

// clusterQuantileP95Ms is the cluster-wide max-across-replicas p95 for a
// histogram family already fetched fleet-wide (TTFT or TPOT), in
// milliseconds - the same MaxBy-then-reduce shape MaxTempC/KVUsageMax use,
// applied per bucket rather than reduced to one scalar.
func clusterQuantileP95Ms(family []tsdb.Series) []tsdb.Point {
	p95 := filterQuantile(family, quantileP95)
	if len(p95) == 0 {
		return nil
	}
	combined := MaxBy(p95, "")
	if len(combined) == 0 {
		return nil
	}
	out := make([]tsdb.Point, len(combined[0].Points))
	for i, p := range combined[0].Points {
		out[i] = tsdb.Point{TimeMS: p.TimeMS, Value: p.Value * 1000}
	}
	return out
}

// rangeDeploymentPresence issues exactly one fleet-wide QueryLatest probe
// (no matcher - every deployment in one call) for metric since sinceMS, and
// reports which "deployment" label values it found at least one sample for.
// This is BuildCluster's "collected somewhere in the wider chart range"
// check (see ClusterSignals.ProxyNoRecent/TTFTNoRecent/TPOTNoRecent): one
// query per metric family regardless of fleet size, never once per
// deployment. QueryLatest only needs to find the newest sample at or after
// sinceMS per series, so this is cheap even over an hours-long range.
func rangeDeploymentPresence(store tsdb.Store, metric string, sinceMS int64) (map[string]bool, error) {
	points, err := store.QueryLatest(metric, nil, sinceMS)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(points))
	for _, p := range points {
		if d := p.Labels["deployment"]; d != "" {
			out[d] = true
		}
	}
	return out, nil
}

func groupInstancesByDeployment(instances []InstanceRef) map[uint][]InstanceRef {
	out := map[uint][]InstanceRef{}
	for _, inst := range instances {
		out[inst.DeploymentID] = append(out[inst.DeploymentID], inst)
	}
	return out
}

func nodeNameIndex(nodes []NodeRef) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.UUID
		}
		out[n.UUID] = name
	}
	return out
}

func resolveNodeNames(dep DeploymentRef, instances []InstanceRef, nodeNames map[string]string) []string {
	if len(dep.NodeNames) > 0 {
		return dep.NodeNames
	}
	seen := map[string]bool{}
	var out []string
	for _, inst := range instances {
		name := nodeNames[inst.NodeUUID]
		if name == "" {
			name = inst.NodeUUID
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// countRunning counts running, API-facing replicas. A headless distributed
// worker (InstanceRef.Headless) is a process-group participant, not a
// replica the product presents to users, so it never contributes to
// ready/desired counts - counting a leader-plus-followers TP/PP group as N
// replicas would overstate capacity by exactly the follower count.
func countRunning(instances []InstanceRef) int {
	n := 0
	for _, inst := range instances {
		if inst.Headless {
			continue
		}
		if inst.State == proto.InstRunning {
			n++
		}
	}
	return n
}

// sortServiceRows orders the rows by name; monitorapi orders them by
// health, which this package does not see.
func sortServiceRows(rows []ServiceRow) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
}

// vramByNode assembles the VRAM three-segment bar per node and reports the
// newest timestamp among the "actual usage" samples it read (zero if none),
// for BuildCluster to fold into AsOf.
func vramByNode(deps Deps, inv Inventory, now time.Time) ([]VRAMNode, time.Time, error) {
	byNode := map[string]*VRAMNode{}
	order := make([]string, 0, len(inv.Nodes))
	for _, n := range inv.Nodes {
		name := n.Name
		if name == "" {
			name = n.UUID
		}
		byNode[n.UUID] = &VRAMNode{NodeUUID: n.UUID, NodeName: name}
		order = append(order, n.UUID)
	}
	for _, g := range inv.GPUs {
		row, ok := byNode[g.NodeUUID]
		if !ok {
			row = &VRAMNode{NodeUUID: g.NodeUUID, NodeName: g.NodeUUID}
			byNode[g.NodeUUID] = row
			order = append(order, g.NodeUUID)
		}
		row.TotalBytes += g.MemoryTotalBytes
		row.AllocatedBytes += g.AllocatedBytes
	}

	var latest time.Time
	since := now.Add(-2 * time.Minute).UnixMilli()
	points, err := deps.TSDB.QueryLatest("gpuworks_gpu_memory_used_bytes", nil, since)
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, p := range points {
		if t := time.UnixMilli(p.TimeMS).UTC(); latest.IsZero() || t.After(latest) {
			latest = t
		}
		node := p.Labels["node"]
		row, ok := byNode[node]
		if !ok {
			continue
		}
		row.UsedBytes += int64(p.Value)
		row.HasUsedBytes = true
		if p.TimeMS > row.UsedAtMS {
			row.UsedAtMS = p.TimeMS
		}
	}

	out := make([]VRAMNode, 0, len(order))
	for _, uuid := range order {
		out = append(out, *byNode[uuid])
	}
	return out, latest, nil
}

func clusterTokenRate(deps Deps, now time.Time, rng time.Duration) ([]tsdb.Point, error) {
	stepMS := ChartStep(rng)
	startMS := now.Add(-rng).UnixMilli()
	endMS := now.UnixMilli()
	genTokens, err := deps.TSDB.QueryRange("gpuworks_infer_generation_tokens_total", nil, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	total := SumBy(Rate(genTokens), "")
	if len(total) == 0 {
		return nil, nil
	}
	return total[0].Points, nil
}

// ChartStep picks a bucket width so a chart over rng has roughly 120
// points, never coarser than the collector's own tick interval.
func ChartStep(rng time.Duration) int64 {
	step := rng / 120
	if step < WindowStep {
		step = WindowStep
	}
	return step.Milliseconds()
}
