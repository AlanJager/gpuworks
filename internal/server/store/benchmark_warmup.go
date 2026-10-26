package store

import "encoding/json"

// Attempts only append. A delayed status must not replace a more complete
// recovery history, especially after cancellation stopped the executor.
func retainBenchmarkWarmup(previous, incoming json.RawMessage) json.RawMessage {
	var oldAttempts, newAttempts []json.RawMessage
	_ = json.Unmarshal(previous, &oldAttempts)
	if json.Unmarshal(incoming, &newAttempts) != nil || len(newAttempts) < len(oldAttempts) {
		return previous
	}
	return incoming
}
