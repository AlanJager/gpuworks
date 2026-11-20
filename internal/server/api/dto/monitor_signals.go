package dto

// MonitorDeploymentSignals is GET /monitor/deployments/{id}/signals: the
// series the deployment drawer's metrics panel draws over a window it picked,
// each signal from the engine when the engine reports it and from the
// gateway otherwise, resolved the way the monitor center's charts are
// (ADR 0039).
//
// A series from the engine carries an "instance" label; one from the
// gateway does not, because the gateway measures what the deployment
// served, not what each instance did. Every list is present, empty when
// there is no data, never null.
type MonitorDeploymentSignals struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
	// StepMS is the bucket width actually used: the requested step, raised
	// when the window would otherwise hold too many points.
	StepMS int64 `json:"step_ms"`
	// Sources says where the running, prompt_tokens and generation_tokens
	// series came from ("engine" or "gateway"); a signal with no data from
	// either is absent. The page does not label the source.
	Sources map[string]string `json:"sources"`
	// Concurrency has labels.metric running (engine or gateway) and waiting
	// (engine only).
	Concurrency []MonitorSeries `json:"concurrency"`
	// KV has labels.metric usage_pct, the engine's cache usage per instance.
	KV []MonitorSeries `json:"kv"`
	// Throughput has labels.metric prompt_tok_s and generation_tok_s.
	Throughput []MonitorSeries `json:"throughput"`
}
