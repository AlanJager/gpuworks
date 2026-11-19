package platformmetrics

import (
	"slices"
	"sort"
)

// exportedDefinitions are served directly in Prometheus text format; they
// are not stored in TSDB or queryable through the monitor metric explorer.
// Histogram entries name the native family, not quantile gauges.
var exportedDefinitions = []Definition{
	{"gpuworks_plugin_up", SourcePlugin, Gauge, "boolean", []string{"id"}, "Whether the installed plugin serves calls now (running or degraded)."},
	{"gpuworks_plugin_state", SourcePlugin, Gauge, "boolean", []string{"id", "state"}, "One for the installed plugin's current state and zero for its other states."},
	{"gpuworks_plugin_restarts_total", SourcePlugin, Counter, "restarts", []string{"id"}, "Automatic resident plugin restarts after a crash or failed health check; excludes on-demand starts."},
	{"gpuworks_plugin_calls_total", SourcePlugin, Counter, "calls", []string{"id", "class", "outcome"}, "Supervised plugin calls by caller class and host outcome; ok includes the plugin's own HTTP refusals."},
	{"gpuworks_plugin_call_seconds", SourcePlugin, Histogram, "seconds", []string{"id", "class"}, "Time from a supervised call's start until response headers or host refusal, including startup wait; buckets also carry le."},
	{"gpuworks_plugin_push_rows_total", SourcePlugin, Counter, "entries", []string{"id", "kind"}, "Entries of pushed tables committed by the writer, including commits whose caller stopped waiting."},
}

// ExportedDefinitions returns scrape-only metric families in series-name
// order. A histogram produces <family>_bucket (with an additional le label),
// <family>_sum and <family>_count, not stored quantiles. The returned slice
// and label lists may be changed without changing the vocabulary.
func ExportedDefinitions() []Definition {
	out := slices.Clone(exportedDefinitions)
	for i := range out {
		out[i].Labels = slices.Clone(out[i].Labels)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Series < out[j].Series })
	return out
}
