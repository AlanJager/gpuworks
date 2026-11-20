package proto

import "encoding/json"

const RPCNetBenchRenew = "net.bench.renew"

// DecodeListenerActionControl retains the initial grant identity and exact
// start bytes for listener renewal or stopping. It uses the existing control
// wire while excluding synchronous observation families.
func DecodeListenerActionControl(raw json.RawMessage) (ObservationActionControlParams, error) {
	return decodeActionControl(raw, IsListenerAction, "listener")
}
