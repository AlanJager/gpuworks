package inferencemetrics

import "sort"

// GatewayPrefix is the stored-series namespace of what the gateway measures
// of the requests it forwards. Those series need no engine cooperation: every
// deployment the gateway serves has them, whatever its engine exports.
const GatewayPrefix = "gpuworks_proxy_"

// GatewayDefinition is one gateway inference metric. Its kinds and units mean
// what they mean for engine metrics; a gateway histogram is in milliseconds,
// as the gateway's request durations always were.
type GatewayDefinition struct {
	Definition
}

// Series is the stored series name.
func (d GatewayDefinition) Series() string { return GatewayPrefix + d.Name }

// gatewayDefinitions are the gateway's inference series. Its operational
// series (routing, retries, the accounting queue) are not inference metrics
// and are not listed here.
var gatewayDefinitions = []GatewayDefinition{
	{Definition{"requests_total", Counter, "requests"}},
	{Definition{"requests_inflight", Gauge, "requests"}},
	// From the request reaching an instance to the response's last byte.
	{Definition{"request_duration_ms", Histogram, "milliseconds"}},
	// From the request reaching an instance to the first generated token of a
	// streamed reply; it includes queueing and the network, unlike an
	// engine's ttft_seconds.
	{Definition{"ttft_ms", Histogram, "milliseconds"}},
	// Token counts the engine reported in the response's usage.
	{Definition{"prompt_tokens_total", Counter, "tokens"}},
	{Definition{"completion_tokens_total", Counter, "tokens"}},
	{Definition{"cached_tokens_total", Counter, "tokens"}},
	// Completed responses of an operation that reports usage that carried
	// none: the tokens above undercount by these requests.
	{Definition{"usage_missing_total", Counter, "requests"}},
	// The fallbacks below (fallback.go) stand in for engine histograms an
	// engine's metrics lack. tpot_ms is the gateway's own: per streamed
	// reply, the time from the first generated token to the last over the
	// tokens after the first.
	{Definition{"tpot_ms", Histogram, "milliseconds"}},
	// The engine's own per-request phase times, read from its replies
	// (request stats) rather than from its exposition.
	{Definition{"reported_queue_ms", Histogram, "milliseconds"}},
	{Definition{"reported_prefill_ms", Histogram, "milliseconds"}},
	{Definition{"reported_decode_ms", Histogram, "milliseconds"}},
	// How long a request the gateway held while its deployment woke waited
	// before it was served, refused or gave up; it counts every held request.
	{Definition{"wake_hold_wait_ms", Histogram, "milliseconds"}},
}

var gatewayByName = func() map[string]GatewayDefinition {
	index := make(map[string]GatewayDefinition, len(gatewayDefinitions))
	for _, definition := range gatewayDefinitions {
		index[definition.Name] = definition
	}
	return index
}()

// Gateway returns the definition of a gateway metric's short name. It panics
// on an unknown name: callers name gateway metrics in code, and a typo must
// fail the first test that runs it rather than store an unlisted series.
func Gateway(name string) GatewayDefinition {
	definition, ok := gatewayByName[name]
	if !ok {
		panic("inferencemetrics: unknown gateway metric " + name)
	}
	return definition
}

// GatewayDefinitions returns the gateway vocabulary sorted by name.
func GatewayDefinitions() []GatewayDefinition {
	out := append([]GatewayDefinition(nil), gatewayDefinitions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
