// Package inferencemetrics is the one definition of the platform's inference
// metrics: their names, kinds and units, the rules that map an engine's
// Prometheus exposition onto them, the built-in rules of the engines the
// platform knows, and the metrics derived from others.
//
// The agent collector that maps a scrape and the catalog that validates a
// template's mapping read it from here; monitor queries and UI panels that
// still spell series names themselves are to move onto it. A template states
// a metric by its short name (ttft_seconds); the stored series is
// SeriesPrefix+name (gpuworks_infer_ttft_seconds), and only this package knows
// that rule.
//
// It is a leaf: it imports nothing from the rest of the repository but the
// engine names, so the agent and the server share it without an edge between
// them. See docs/adr/0039-inference-metric-sources.md.
package inferencemetrics

import "sort"

// Kind is how a metric accumulates.
type Kind string

const (
	// Counter only grows; consumers derive rates over a window.
	Counter Kind = "counter"
	// Gauge is a point-in-time value.
	Gauge Kind = "gauge"
	// Histogram is a Prometheus histogram family: the stored series are
	// <name>_bucket{le}, <name>_sum and <name>_count.
	Histogram Kind = "histogram"
)

// SeriesPrefix is the stored-series namespace of every inference metric.
const SeriesPrefix = "gpuworks_infer_"

// Definition is one platform inference metric.
type Definition struct {
	// Name is the short name templates and rules use.
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Unit is what one unit of the value is: requests, tokens, ratio (0..1),
	// seconds, bytes or events.
	Unit string `json:"unit"`
}

// Series is the stored series name.
func (d Definition) Series() string { return SeriesPrefix + d.Name }

// definitions is the vocabulary. A name already stored keeps its spelling even
// where it reads oddly (gpu_cache_usage_percent holds a 0..1 ratio, rendered as
// a percentage): renaming it would orphan the stored history.
var definitions = []Definition{
	{"requests_running", Gauge, "requests"},
	{"requests_waiting", Gauge, "requests"},
	{"requests_success_total", Counter, "requests"},
	{"preemptions_total", Counter, "events"},
	{"prompt_tokens_total", Counter, "tokens"},
	{"generation_tokens_total", Counter, "tokens"},
	{"prompt_tokens_external_total", Counter, "tokens"},
	{"prompt_tokens_local_compute_total", Counter, "tokens"},
	{"prompt_tokens_cache_hit_total", Counter, "tokens"},
	{"gpu_cache_usage_percent", Gauge, "ratio"},
	// KV pool size and free space, for an engine that reports those instead
	// of a usage ratio; gpu_cache_usage_percent is derived from them.
	{"kv_capacity_tokens", Gauge, "tokens"},
	{"kv_free_tokens", Gauge, "tokens"},
	{"kv_queries_total", Counter, "tokens"},
	{"kv_hits_total", Counter, "tokens"},
	{"kv_hit_rate_instant", Gauge, "ratio"},
	{"kv_memory_queries_total", Counter, "events"},
	{"kv_memory_hits_total", Counter, "events"},
	{"kv_disk_queries_total", Counter, "events"},
	{"kv_disk_hits_total", Counter, "events"},
	{"kv_disk_write_bytes_total", Counter, "bytes"},
	{"kv_disk_write_failures_total", Counter, "events"},
	{"spec_draft_tokens_total", Counter, "tokens"},
	{"spec_accepted_tokens_total", Counter, "tokens"},
	{"spec_accept_rate", Gauge, "ratio"},
	{"spec_accept_length", Gauge, "tokens"},
	{"ttft_seconds", Histogram, "seconds"},
	{"tpot_seconds", Histogram, "seconds"},
	{"e2e_seconds", Histogram, "seconds"},
	{"queue_time_seconds", Histogram, "seconds"},
	{"prefill_time_seconds", Histogram, "seconds"},
	{"decode_time_seconds", Histogram, "seconds"},
	{"nixl_transfer_seconds", Histogram, "seconds"},
	{"nixl_transfer_bytes", Histogram, "bytes"},
	{"nixl_failed_transfers_total", Counter, "events"},
	{"nixl_kv_expired_total", Counter, "events"},
}

var byName = func() map[string]Definition {
	index := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		index[definition.Name] = definition
	}
	return index
}()

// Lookup returns the definition of a short name.
func Lookup(name string) (Definition, bool) {
	definition, ok := byName[name]
	return definition, ok
}

// Definitions returns the vocabulary sorted by name.
func Definitions() []Definition {
	out := append([]Definition(nil), definitions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every short name, sorted.
func Names() []string {
	out := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, definition.Name)
	}
	sort.Strings(out)
	return out
}
