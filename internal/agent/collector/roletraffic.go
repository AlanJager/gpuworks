package collector

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// roleSampler turns the agent's per-role byte counters (package traffic)
// into rates, reported from this node's side. Serving traffic is counted by
// the server's gateway instead, under the same metric names.
type roleSampler struct {
	totals func() map[string][2]uint64
	prev   map[string][2]uint64
	prevAt time.Time
}

func (r *roleSampler) sample(node string, at time.Time, now int64) []proto.MetricSample {
	if r.totals == nil {
		return nil
	}
	cur := r.totals()
	prev, prevAt := r.prev, r.prevAt
	r.prev, r.prevAt = cur, at
	dt := at.Sub(prevAt).Seconds()
	if prev == nil || dt <= 0 {
		return nil
	}
	roles := make([]string, 0, len(cur))
	for role := range cur {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var out []proto.MetricSample
	for _, role := range roles {
		before, ok := prev[role]
		if !ok {
			continue
		}
		after := cur[role]
		labels := map[string]string{"node": node, "role": role}
		out = append(out,
			proto.MetricSample{Metric: "gpuworks_node_role_rx_bytes_per_sec", Labels: labels, Value: float64(subUint64(after[0], before[0])) / dt, TimeMS: now},
			proto.MetricSample{Metric: "gpuworks_node_role_tx_bytes_per_sec", Labels: labels, Value: float64(subUint64(after[1], before[1])) / dt, TimeMS: now},
		)
	}
	return out
}
