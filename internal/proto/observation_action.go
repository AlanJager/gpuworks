package proto

// IsObservationAction identifies synchronous RPCs that actively contact peers
// or run diagnostic work. Their grants bind the registered node and exact
// request bytes; these operations do not have a persistent desired version.
// Listener creation has a separate lifetime after its RPC returns.
func IsObservationAction(method string) bool {
	switch method {
	case RPCBenchProbe, RPCNetProbe, RPCNodeDoctor:
		return true
	default:
		return false
	}
}
