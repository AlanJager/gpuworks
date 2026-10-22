package inferencemetrics

// GatewayFallback is a gateway histogram that stands in for an engine
// histogram an engine's metrics lack, so a deployment of an engine that
// exports no TPOT or phase times still has them (TensorFold, Strata, a
// callable engine). It is recorded only for engines whose built-in mapping
// lacks the engine metric: vLLM and SGLang export their own and would only
// write the same figure twice.
type GatewayFallback struct {
	// Gateway is the gateway metric's short name (GatewayDefinitions).
	Gateway string
	// Engine is the engine metric it stands in for (vocabulary.go).
	Engine string
	// Stat is the request stat it is read from; empty for the gateway's own
	// measurement (TPOT).
	Stat string
}

var gatewayFallbacks = []GatewayFallback{
	{Gateway: "tpot_ms", Engine: "tpot_seconds"},
	{Gateway: "reported_queue_ms", Engine: "queue_time_seconds", Stat: RequestQueueSeconds},
	{Gateway: "reported_prefill_ms", Engine: "prefill_time_seconds", Stat: RequestPrefillSeconds},
	{Gateway: "reported_decode_ms", Engine: "decode_time_seconds", Stat: RequestDecodeSeconds},
}

// GatewayFallbacks returns every fallback, in a fixed order.
func GatewayFallbacks() []GatewayFallback {
	return append([]GatewayFallback(nil), gatewayFallbacks...)
}

// GatewayFallbacksFor returns the fallbacks the gateway records for an
// engine: those whose engine metric the engine's built-in mapping does not
// provide. An engine without one (callable) gets them all.
func GatewayFallbacksFor(engine string) []GatewayFallback {
	mapped := map[string]bool{}
	for _, rule := range engineRules[engine] {
		mapped[rule.Metric] = true
	}
	var out []GatewayFallback
	for _, fallback := range gatewayFallbacks {
		if !mapped[fallback.Engine] {
			out = append(out, fallback)
		}
	}
	return out
}
