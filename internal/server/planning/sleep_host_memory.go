package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// HostMemory reads a node's host memory as last observed: what is available
// now and the total. ok is false when there is no recent observation.
type HostMemory func(nodeUUID string) (availableBytes, totalBytes int64, ok bool)

// HostMemoryFromMetrics reads a node's host memory from the host series its
// agent reports: latest answers a node's newest recent sample of a metric.
// Available is the total less what is in use.
func HostMemoryFromMetrics(latest func(metric, nodeUUID string) (float64, bool)) HostMemory {
	return func(nodeUUID string) (int64, int64, bool) {
		total, okTotal := latest("gpuworks_node_mem_total_bytes", nodeUUID)
		used, okUsed := latest("gpuworks_node_mem_used_bytes", nodeUUID)
		if !okTotal || !okUsed || total <= 0 {
			return 0, 0, false
		}
		return int64(total - used), int64(total), true
	}
}

// L1HostMemoryRefusal is why an L1 sleep of the named instances may not
// proceed, nil when it may: on some node, the weights the awake instances
// there would move into host memory exceed what the host has available less
// its headroom (sleep.HostMemoryHeadroom). Each rank of a multi-node replica
// moves its own share of the weights onto its own host. A node with no recent
// host memory observation is not refused: the platform does not know, and
// the operator still decides.
func L1HostMemoryRefusal(st *store.Store, dep *store.Deployment, named []store.Instance, host HostMemory) *SleepRefusal {
	if host == nil || len(named) == 0 {
		return nil
	}
	model, err := st.GetModel(dep.ModelID)
	if err != nil || model.WeightBytes <= 0 {
		return nil
	}
	need := map[uint]int64{}
	var order []uint
	for i := range named {
		in := &named[i]
		if in.SleepLevel > 0 || !proto.InstanceFillsReplica(in.State) {
			continue
		}
		share := model.WeightBytes / int64(max(in.NNodes, 1))
		if _, seen := need[in.NodeID]; !seen {
			order = append(order, in.NodeID)
		}
		need[in.NodeID] += share
	}
	for _, id := range order {
		node, err := st.NodeByID(id)
		if err != nil {
			continue
		}
		available, total, ok := host(node.UUID)
		if !ok {
			continue
		}
		if need[id] > available-sleep.HostMemoryHeadroom(total) {
			return &SleepRefusal{Code: sleep.CodeHostMemoryInsufficient, Params: map[string]any{
				"node":          node.DisplayName(),
				"need_gib":      fmt.Sprintf("%.1f", float64(need[id])/(1<<30)),
				"available_gib": fmt.Sprintf("%.1f", float64(available)/(1<<30)),
			}}
		}
	}
	return nil
}
