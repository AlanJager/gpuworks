package monitor

import (
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// GPU metrics a replica row reports per GPU. Each is fetched once per page,
// not once per (replica, GPU): tsdb pushes only the "instance" and
// "deployment" matchers into SQL, so a node/gpu-matched query scans and
// decodes every series of the metric in the fleet.
const (
	metricGPUUtilization = "gpuworks_gpu_utilization"
	metricGPUTemperature = "gpuworks_gpu_temperature_celsius"
	metricGPUPower       = "gpuworks_gpu_power_watts"
)

// gpuKey identifies one card by the "node" and "gpu" label values its
// hardware series carry.
type gpuKey struct{ node, gpu string }

// gpuSeriesIndex holds one GPU metric's series for the cards a page shows,
// grouped by card.
type gpuSeriesIndex map[gpuKey][]tsdb.Series

// instanceSeries groups series by their "instance" label value.
func instanceSeries(series []tsdb.Series) map[string][]tsdb.Series {
	out := make(map[string][]tsdb.Series)
	for _, s := range series {
		if id := s.Labels["instance"]; id != "" {
			out[id] = append(out[id], s)
		}
	}
	return out
}

// queryGPUSeries fetches metric once and keeps the series of the wanted
// cards.
func queryGPUSeries(store tsdb.Store, metric string, wanted map[gpuKey]bool, startMS, endMS, stepMS int64) (gpuSeriesIndex, error) {
	series, err := store.QueryRange(metric, nil, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	out := make(gpuSeriesIndex)
	for _, s := range series {
		key := gpuKey{node: s.Labels["node"], gpu: s.Labels["gpu"]}
		if wanted[key] {
			out[key] = append(out[key], s)
		}
	}
	return out, nil
}

// replicaGPUSeries is the four GPU metrics of the cards a page shows.
type replicaGPUSeries struct {
	util, memBW, temp, power gpuSeriesIndex
}

// fetchReplicaGPUSeries queries each GPU metric once for every card the
// replicas use; it queries nothing when no replica has a GPU.
func fetchReplicaGPUSeries(store tsdb.Store, replicas []InstanceRef, startMS, endMS, stepMS int64) (replicaGPUSeries, error) {
	wanted := make(map[gpuKey]bool)
	for _, inst := range replicas {
		for _, idx := range inst.GPUIndexes {
			wanted[gpuKey{node: inst.NodeUUID, gpu: fmt.Sprintf("%d", idx)}] = true
		}
	}
	var out replicaGPUSeries
	if len(wanted) == 0 {
		return out, nil
	}
	for _, f := range []struct {
		metric string
		dst    *gpuSeriesIndex
	}{
		{metricGPUUtilization, &out.util},
		{metricGPUMemBW, &out.memBW},
		{metricGPUTemperature, &out.temp},
		{metricGPUPower, &out.power},
	} {
		idx, err := queryGPUSeries(store, f.metric, wanted, startMS, endMS, stepMS)
		if err != nil {
			return replicaGPUSeries{}, err
		}
		*f.dst = idx
	}
	return out, nil
}

// buildReplicaRows builds the replica table for replicas (the deployment's
// non-headless instances). The inference signals are sliced by instance out
// of engine, the deployment's engine series already fetched for the window
// over the same range and step; the GPU signals take one query per metric
// for all the cards. The query count is therefore independent of the
// replica and GPU count (the page used to issue ~8 queries per replica plus
// 4 per GPU).
func buildReplicaRows(store tsdb.Store, replicas []InstanceRef, engine WindowSeries, nodeNames map[string]string, now time.Time) ([]ReplicaRow, error) {
	if len(replicas) == 0 {
		return nil, nil
	}
	startMS := now.Add(-DiagnosisWindow).UnixMilli()
	endMS := now.UnixMilli()
	stepMS := WindowStep.Milliseconds()

	gpus, err := fetchReplicaGPUSeries(store, replicas, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	infer := replicaInferSeries{
		running:      instanceSeries(engine.Running),
		waiting:      instanceSeries(engine.Waiting),
		kvUsage:      instanceSeries(engine.KVUsage),
		kvQueries:    instanceSeries(engine.KVQueries),
		kvHits:       instanceSeries(engine.KVHits),
		kvInstant:    instanceSeries(engine.KVInstant),
		promptTokens: instanceSeries(engine.PromptTokens),
		genTokens:    instanceSeries(engine.GenTokens),
	}
	rows := make([]ReplicaRow, 0, len(replicas))
	for _, inst := range replicas {
		row := ReplicaRow{
			InstanceUUID: inst.UUID,
			NodeUUID:     inst.NodeUUID,
			NodeName:     nodeNameOrUUID(nodeNames, inst.NodeUUID),
			State:        inst.State,
			Role:         inst.Role,
		}
		infer.fill(&row, inst.UUID)
		for _, idx := range inst.GPUIndexes {
			row.GPUs = append(row.GPUs, gpus.row(gpuKey{node: inst.NodeUUID, gpu: fmt.Sprintf("%d", idx)}, idx))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// replicaInferSeries is the engine's per-instance series of one deployment,
// grouped by instance.
type replicaInferSeries struct {
	running, waiting, kvUsage, kvQueries, kvHits, kvInstant, promptTokens, genTokens map[string][]tsdb.Series
}

// fill sets the inference fields of row from the series of instance uuid.
func (s replicaInferSeries) fill(row *ReplicaRow, uuid string) {
	running, waiting, kvUsage := s.running[uuid], s.waiting[uuid], s.kvUsage[uuid]
	if len(running) > 0 {
		if combined := SumBy(running, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasRunning, row.Running = true, Mean(Values(combined[0]))
		}
	}
	if len(waiting) > 0 {
		if combined := SumBy(waiting, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasWaiting, row.Waiting = true, Mean(Values(combined[0]))
		}
	}
	if len(kvUsage) > 0 {
		if combined := MaxBy(kvUsage, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			row.HasKVUsage, row.KVUsage = true, MaxOf(Values(combined[0]))
		}
	}

	var kv Window
	computeKVHitRate(&kv, s.kvQueries[uuid], s.kvHits[uuid], s.kvInstant[uuid])
	row.HasKVHitRate, row.KVHitRate = kv.HasKVHitRate, kv.KVHitRate

	promptTokens, genTokens := s.promptTokens[uuid], s.genTokens[uuid]
	row.HasTokens = len(promptTokens) > 0 || len(genTokens) > 0
	if combined := SumBy(Rate(promptTokens), ""); len(combined) > 0 && len(combined[0].Points) > 0 {
		row.PromptTokensPerSec = Mean(Values(combined[0]))
	}
	if combined := SumBy(Rate(genTokens), ""); len(combined) > 0 && len(combined[0].Points) > 0 {
		row.GenTokensPerSec = Mean(Values(combined[0]))
	}
}

// row reduces card key's four series to a ReplicaGPU for GPU index idx.
func (g replicaGPUSeries) row(key gpuKey, idx int) ReplicaGPU {
	gpu := ReplicaGPU{Index: idx}
	if util := g.util[key]; len(util) > 0 {
		if combined := AvgBy(util, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			gpu.HasUtil, gpu.UtilPct = true, Mean(Values(combined[0]))
		}
	}
	if memBW := g.memBW[key]; len(memBW) > 0 {
		if combined := AvgBy(memBW, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			gpu.HasMemBW, gpu.MemBWPct = true, Mean(Values(combined[0]))
		}
	}
	if temp := g.temp[key]; len(temp) > 0 {
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
	if power := g.power[key]; len(power) > 0 {
		if combined := AvgBy(power, ""); len(combined) > 0 && len(combined[0].Points) > 0 {
			gpu.HasPower, gpu.PowerW = true, Mean(Values(combined[0]))
		}
	}
	return gpu
}
