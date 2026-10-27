package planning

import "gpuworks.ai/gpuworks/internal/server/store"

// OnlineInstances selects observations whose actual node is connected. Use this
// for serving availability, never for reconciliation: a disconnected instance
// still occupies its unit until the reconnect grace period expires.
func OnlineInstances(instances []store.Instance, online func(uint) bool) []store.Instance {
	out := make([]store.Instance, 0, len(instances))
	for _, inst := range instances {
		if online(inst.NodeID) {
			out = append(out, inst)
		}
	}
	return out
}
