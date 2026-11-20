package monitor

import (
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// PanelSignals are the series the deployment drawer's metrics panel draws
// (concurrency, cache usage, token throughput), over a window the caller
// picked rather than one of the monitor center's fixed ranges. They are
// resolved exactly like the monitor center's charts: the signals the
// gateway can stand in for (window_sources.go) come from the engine when it
// reports them and from the gateway otherwise, so the drawer and the monitor
// center never disagree about a deployment whose engine reports some signals
// and not others.
//
// A series from the engine is per instance (labelled "instance"); one from
// the gateway is the deployment's alone, because the gateway counts what it
// forwards to the deployment, not to each instance, and carries no instance
// label. The panel never gets a per-instance figure the platform cannot
// measure.
type PanelSignals struct {
	// Sources says, for each of the running, prompt_tokens and
	// generation_tokens signals, where its series came from; a signal with no
	// data from either source is absent. Waiting requests and cache usage
	// are engine-only and have no entry.
	Sources map[Signal]Source
	// Charts are the lines by chart, named as BuildService names them:
	// "concurrency" (running, and waiting only when the engine reports it),
	// "kv" (usage, a 0..1 fraction) and "throughput" (token rates).
	Charts map[string][]ChartSeries
}

// panelSources are the signals PanelSignals reports a source for: the ones
// of the catalog it fetches.
var panelSources = []Signal{SignalRunning, SignalPromptTokens, SignalGenerationTokens}

// BuildPanelSignals resolves one deployment's PanelSignals over
// [startMS, endMS] at stepMS. It queries only what the panel draws, by the
// deployment label (instances the deployment label does not cover are read
// by their own, as BuildService does).
func BuildPanelSignals(store tsdb.Store, deploymentID uint, instances []InstanceRef, startMS, endMS, stepMS int64) (PanelSignals, error) {
	depKey := depKeyString(deploymentID)
	running, uncovered, err := queryRunningWithCoverage(store, depKey, instances, startMS, endMS, stepMS)
	if err != nil {
		return PanelSignals{}, err
	}
	query := func(metric string) ([]tsdb.Series, error) {
		return queryDeploymentMetric(store, metric, depKey, uncovered, startMS, endMS, stepMS)
	}
	waiting, err := query("gpuworks_infer_requests_waiting")
	if err != nil {
		return PanelSignals{}, err
	}
	kvUsage, err := query("gpuworks_infer_gpu_cache_usage_percent")
	if err != nil {
		return PanelSignals{}, err
	}
	promptTokens, err := query("gpuworks_infer_prompt_tokens_total")
	if err != nil {
		return PanelSignals{}, err
	}
	genTokens, err := query("gpuworks_infer_generation_tokens_total")
	if err != nil {
		return PanelSignals{}, err
	}
	resolved := WindowSeries{Running: running, PromptTokens: promptTokens, GenTokens: genTokens}
	// The gateway's stand-ins are fetched only when the engine left a signal
	// empty: a deployment whose engine reports it all costs nothing extra.
	if !hasPoints(running) || !hasPoints(promptTokens) || !hasPoints(genTokens) {
		matcher := map[string]string{"deployment": depKey}
		if resolved.ProxyInflight, err = store.QueryRange(metricProxyInflight, matcher, startMS, endMS, stepMS); err != nil {
			return PanelSignals{}, err
		}
		if resolved.Gateway, err = queryGatewaySubset(store, matcher, startMS, endMS, stepMS, metricGatewayPromptTokens, metricGatewayCompletionTokens); err != nil {
			return PanelSignals{}, err
		}
	}
	resolved.resolveSources()
	charts := map[string][]ChartSeries{}
	charts["concurrency"] = named("running", SumBy(resolved.Running, "instance"))
	charts["concurrency"] = append(charts["concurrency"], named("waiting", SumBy(waiting, "instance"))...)
	charts["kv"] = named("usage", MaxBy(kvUsage, "instance"))
	charts["throughput"] = named("prompt_tokens_per_sec", SumBy(Rate(resolved.PromptTokens), "instance"))
	charts["throughput"] = append(charts["throughput"], named("generation_tokens_per_sec", SumBy(Rate(resolved.GenTokens), "instance"))...)
	out := PanelSignals{Sources: map[Signal]Source{}, Charts: charts}
	for _, signal := range panelSources {
		if source, ok := resolved.Sources[signal]; ok {
			out.Sources[signal] = source
		}
	}
	return out, nil
}

// named is namedSeries for a series grouped by instance, minus the empty
// "instance" label the grouping gives a series that has none (the gateway's):
// such a line belongs to the deployment, and says so by not naming an
// instance.
func named(name string, series []tsdb.Series) []ChartSeries {
	out := namedSeries(name, series)
	for i := range out {
		if out[i].Labels["instance"] == "" {
			labels := make(map[string]string, len(out[i].Labels))
			for k, v := range out[i].Labels {
				if k != "instance" {
					labels[k] = v
				}
			}
			out[i].Labels = labels
		}
	}
	return out
}
