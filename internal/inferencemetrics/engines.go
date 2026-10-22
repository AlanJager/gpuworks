package inferencemetrics

import "gpuworks.ai/gpuworks/internal/engines"

// engineRules are the mappings of the engines whose exposition the platform
// knows. A deployment of that engine uses them unless its template states a
// mapping of its own; a template inherits one by name (api.metrics.inherit);
// and a callable deployment that states none is matched against them by
// family prefix (DetectSets). This is their only definition: the deploy
// engine profiles derive their MetricMap view from here.
var engineRules = map[string][]Rule{
	engines.VLLM: {
		{Source: "vllm:num_requests_running", Metric: "requests_running"},
		{Source: "vllm:num_requests_waiting", Metric: "requests_waiting"},
		// vLLM renamed the cache usage gauge from gpu_cache_usage_perc to
		// kv_cache_usage_perc (old name removed per its deprecation policy);
		// both map for old and new images. The gauge is a 0..1 fraction
		// ("1 means 100 percent"), scaled to percent at render.
		{Source: "vllm:kv_cache_usage_perc", Metric: "gpu_cache_usage_percent"},
		{Source: "vllm:gpu_cache_usage_perc", Metric: "gpu_cache_usage_percent"},
		{Source: "vllm:generation_tokens_total", Metric: "generation_tokens_total"},
		{Source: "vllm:prompt_tokens_total", Metric: "prompt_tokens_total"},
		// Counters carry the _total suffix prometheus_client appends in
		// exposition. The window ratios (KV hit rate, spec accept rate) are
		// derived from these counter pairs at display time, never stored.
		// Pre-V1 engines only exposed a lifetime-average gauge; it is
		// deliberately not mapped because it cannot yield a window rate.
		{Source: "vllm:prefix_cache_queries_total", Metric: "kv_queries_total"},
		{Source: "vllm:prefix_cache_hits_total", Metric: "kv_hits_total"},
		{Source: "vllm:spec_decode_num_draft_tokens_total", Metric: "spec_draft_tokens_total"},
		{Source: "vllm:spec_decode_num_accepted_tokens_total", Metric: "spec_accepted_tokens_total"},
		// KV offload tiering. One exposition name serves every tier and the
		// tier label picks the row: "0:primary" is the memory tier and
		// "1:fs" the single filesystem tier the platform configures
		// (vllm/v1/kv_offload/tiering/metrics.py labels tier i as
		// "{i+1}:{type}"). Each tier gets its own platform metric, because
		// the samples reaching the time series carry only node/instance/engine.
		// An image without the tiering connector exposes none of these, and
		// the instance panel says so rather than showing a zero.
		{Source: `vllm:kv_offload_tiering_chunk_queries_total{tier="0:primary"}`, Metric: "kv_memory_queries_total"},
		{Source: `vllm:kv_offload_tiering_chunk_hits_total{tier="0:primary"}`, Metric: "kv_memory_hits_total"},
		{Source: `vllm:kv_offload_tiering_chunk_queries_total{tier="1:fs"}`, Metric: "kv_disk_queries_total"},
		{Source: `vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}`, Metric: "kv_disk_hits_total"},
		{Source: `vllm:kv_offload_tiering_write_bytes_total{tier="1:fs"}`, Metric: "kv_disk_write_bytes_total"},
		{Source: `vllm:kv_offload_tiering_cascade_job_failures_total{tier="1:fs"}`, Metric: "kv_disk_write_failures_total"},
		// Request-latency histograms and their counterparts (plan §4.2,
		// phase-2 metrics contract). The collector expands a mapped
		// histogram family into <metric>_bucket{le}/_sum/_count from the
		// matching _bucket/_sum/_count exposition lines; the bare name
		// below is the family, not any one exposition line.
		{Source: "vllm:time_to_first_token_seconds", Metric: "ttft_seconds"},
		// vLLM >= 0.10 renamed time_per_output_token_seconds to
		// inter_token_latency_seconds (confirmed on a real vLLM 0.25.1
		// /metrics: only the new name is exposed with default flags). Both
		// map to the same family; the collector dedups by summing duplicate
		// families within one scrape. Normally a real engine only ever
		// exposes one of the two names, so this never runs - but with
		// --show-hidden-metrics-for-version set to the old release the
		// deprecated name reappears alongside the new one, and both get
		// scraped. The quantile gauges this family feeds are unaffected (the
		// two histograms carry identical data, so summing them is a uniform
		// 2x scale on every bucket, which histogram_quantile's interpolation
		// cancels out) but _sum/_count do double count in that case.
		{Source: "vllm:time_per_output_token_seconds", Metric: "tpot_seconds"},
		{Source: "vllm:inter_token_latency_seconds", Metric: "tpot_seconds"},
		{Source: "vllm:e2e_request_latency_seconds", Metric: "e2e_seconds"},
		{Source: "vllm:request_queue_time_seconds", Metric: "queue_time_seconds"},
		{Source: "vllm:request_prefill_time_seconds", Metric: "prefill_time_seconds"},
		{Source: "vllm:request_decode_time_seconds", Metric: "decode_time_seconds"},
		{Source: "vllm:num_preemptions_total", Metric: "preemptions_total"},
		{Source: "vllm:request_success_total", Metric: "requests_success_total"},
		// NIXL KV transfer (PD disaggregation), from
		// vllm/distributed/kv_transfer/kv_connector/v1/nixl/stats.py and
		// confirmed on vllm/vllm-openai:v0.29.0 with NixlConnector P/D
		// instances (2026-09-26). The bytes histogram's _sum is the bytes
		// moved and its _count the transfers; failed transfers include
		// handshake and notification failures. An image without the NIXL
		// connector exposes none of these.
		{Source: "vllm:nixl_xfer_time_seconds", Metric: "nixl_transfer_seconds"},
		{Source: "vllm:nixl_bytes_transferred", Metric: "nixl_transfer_bytes"},
		{Source: "vllm:nixl_num_failed_transfers_total", Metric: "nixl_failed_transfers_total"},
		{Source: "vllm:nixl_num_kv_expired_reqs_total", Metric: "nixl_kv_expired_total"},
		// Where a request's prompt tokens came from. On a PD decoder the
		// external share is the PD effectiveness (monitor/window_pd.go);
		// confirmed on vLLM 0.25.1 (phase 0 record).
		{Source: `vllm:prompt_tokens_by_source_total{source="external_kv_transfer"}`, Metric: "prompt_tokens_external_total"},
		{Source: `vllm:prompt_tokens_by_source_total{source="local_compute"}`, Metric: "prompt_tokens_local_compute_total"},
		{Source: `vllm:prompt_tokens_by_source_total{source="local_cache_hit"}`, Metric: "prompt_tokens_cache_hit_total"},
	},
	engines.SGLang: {
		{Source: "sglang:num_running_reqs", Metric: "requests_running"},
		{Source: "sglang:num_queue_reqs", Metric: "requests_waiting"},
		{Source: "sglang:token_usage", Metric: "gpu_cache_usage_percent"},
		{Source: "sglang:generation_tokens_total", Metric: "generation_tokens_total"},
		{Source: "sglang:prompt_tokens_total", Metric: "prompt_tokens_total"},
		{Source: "sglang:num_requests_running", Metric: "requests_running"},
		{Source: "sglang:num_requests_waiting", Metric: "requests_waiting"},
		// Instantaneous engine gauges, not cumulative counters: the KV
		// value keeps a distinct _instant name so it is never presented as
		// the window ratio computed from vLLM's counter pair.
		{Source: "sglang:cache_hit_rate", Metric: "kv_hit_rate_instant"},
		{Source: "sglang:spec_accept_rate", Metric: "spec_accept_rate"},
		{Source: "sglang:spec_accept_length", Metric: "spec_accept_length"},
		// Request-latency histograms and the preemption counter are
		// SOURCE-VERIFIED, NOT IMAGE-VERIFIED: confirmed present against
		// sgl-project/sglang@main
		// python/sglang/srt/observability/metrics_collector.py (fetched
		// 2026-09-19; the module moved there from srt/metrics/collector.py),
		// but never scraped from an actual running lmsysorg/sglang
		// container. Plan §11 calls for real-image confirmation before
		// this mapping is fully trusted; test/fake-engine's sglang dialect
		// exercises the collector/tsdb path against these exact names in
		// the meantime, which is not the same as a real engine agreeing.
		// Only names actually declared in that source file are mapped:
		//   - sglang:time_to_first_token_seconds (Histogram)
		//   - sglang:inter_token_latency_seconds (Histogram; this replaced
		//     the deprecated sglang:time_per_output_token_seconds)
		//   - sglang:e2e_request_latency_seconds (Histogram)
		//   - sglang:queue_time_seconds (Histogram) - the contract draft
		//     had "(none known)" for queue time; the source has one.
		//   - sglang:num_retracted_requests_total (Counter) - SGLang's
		//     retract is the preemption analog vLLM calls num_preemptions.
		// Deliberately left unmapped (source checked, not just absent from
		// memory):
		//   - prefill/decode time: only sglang:per_stage_req_latency_seconds
		//     exists, keyed by a "stage" label whose exact values were not
		//     recoverable from a diff-only source read; mapping the wrong
		//     stage value would silently mislabel data, worse than showing
		//     "needs upgrade".
		//   - request_success_total: the closest counter,
		//     sglang:num_requests_total, carries a mandatory "is_streaming"
		//     label (no bare/unlabeled row), so a single rule cannot sum both
		//     label values into one platform series.
		{Source: "sglang:time_to_first_token_seconds", Metric: "ttft_seconds"},
		{Source: "sglang:inter_token_latency_seconds", Metric: "tpot_seconds"},
		{Source: "sglang:e2e_request_latency_seconds", Metric: "e2e_seconds"},
		{Source: "sglang:queue_time_seconds", Metric: "queue_time_seconds"},
		{Source: "sglang:num_retracted_requests_total", Metric: "preemptions_total"},
	},
}

// EngineRules returns the built-in mapping of an engine, or nil for an engine
// whose exposition the platform does not know (callable).
func EngineRules(engine string) []Rule {
	rules, ok := engineRules[engine]
	if !ok {
		return nil
	}
	return append([]Rule(nil), rules...)
}

// HasEngineRules reports whether engine has a built-in mapping, which is what
// a template may inherit.
func HasEngineRules(engine string) bool {
	_, ok := engineRules[engine]
	return ok
}

// RuleSet is a mapping chosen by the exposition family prefix it applies to.
type RuleSet struct {
	// Prefix is matched as Prefix+":" at the start of a family name.
	Prefix string
	Rules  []Rule
}

// DetectSets are the mappings an undeclared callable deployment is matched
// against, in order: the first whose prefix appears in a scrape applies. A
// vLLM or SGLang derivative that keeps its upstream metric names needs no
// declaration.
func DetectSets() []RuleSet {
	return []RuleSet{
		{Prefix: engines.VLLM, Rules: EngineRules(engines.VLLM)},
		{Prefix: engines.SGLang, Rules: EngineRules(engines.SGLang)},
	}
}
