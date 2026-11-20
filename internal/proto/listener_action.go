package proto

// IsListenerAction identifies RPC admission whose execution continues after
// the start response. It must never inherit synchronous observation completion.
func IsListenerAction(method string) bool {
	return method == RPCNetBenchStart
}
