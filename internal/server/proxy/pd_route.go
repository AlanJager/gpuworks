// pd_route.go is a PD deployment's routing state in the gateway: its mode,
// its ready prefill instances, which instances may take new work, and the
// handoffs a draining prefill instance still holds.

package proxy

import (
	"fmt"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pdRoute is a PD deployment's routing state in one snapshot: its mode, its
// fallback policy and its ready prefill instances. The decode instances are
// the deployment's ordinary candidates.
type pdRoute struct {
	mode      pdmode.Mode
	fallback  string
	placement *planning.PDPlacement
	prefill   []candidate
	// asleepPrefill counts the running prefill instances left out of prefill
	// because they sleep.
	asleepPrefill int
}

// pdRouteFor reads a deployment's PD placement; nil for any other
// deployment. A PD placement the gateway cannot read, or whose mode it does
// not know, is an error: routing its instances as ordinary replicas would
// send requests to prefill instances as if they could answer them.
func pdRouteFor(dep *store.Deployment) (*pdRoute, error) {
	if dep == nil {
		return nil, nil
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		return nil, err
	}
	mode, ok := pdmode.Lookup(pd.Mode)
	if !ok {
		return nil, fmt.Errorf("unknown PD mode %q", pd.Mode)
	}
	return &pdRoute{mode: mode, fallback: pd.EffectiveFallback(), placement: pd}, nil
}

// wantsPrefill reports whether the deployment asks for prefill instances at
// all. A deployment scaled to no prefill instance serves every request from
// its decoders by design (plan D14): that is not a fallback and is not
// counted as one.
// A deployment whose prefill instances all sleep is served the same way when
// its mode may run without prefill: the sleep was ordered, so its decoders
// answering alone is not a degradation.
func (r *pdRoute) wantsPrefill() bool {
	if len(r.prefill) == 0 && r.asleepPrefill > 0 && r.mode.MayBeEmpty(pdmode.RolePrefill) {
		return false
	}
	return len(r.placement.Units(pdmode.RolePrefill)) > 0
}

// noteAsleep records a running instance left out of routing because it
// sleeps.
func (r *pdRoute) noteAsleep(inst *store.Instance) {
	if inst.Role == pdmode.RolePrefill {
		r.asleepPrefill++
	}
}

// draining returns the running instances no unit of the placement claims:
// they finish what they hold and take nothing new while the controller
// waits to remove them. The claim is planning.MatchUnits, the same one the
// controller uses.
func (r *pdRoute) draining(running []store.Instance, nodes map[uint]string) map[string]bool {
	out := map[string]bool{}
	for _, inst := range planning.MatchUnits(r.placement.UnitsOf(r.mode), running, nodes, "").Excess {
		out[inst.UUID] = true
	}
	return out
}

// withoutUnroutable drops from the decode candidates and the prefill list
// the draining instances and the multi-node leaders whose followers are not
// all running, returning the decode candidates that may take new work.
// instances holds the deployment's running instances, followers included.
func (r *pdRoute) withoutUnroutable(decode []candidate, instances []store.Instance, nodes map[uint]string) []candidate {
	drop := r.draining(instances, nodes)
	for uuid := range planning.GroupsIncomplete(instances) {
		drop[uuid] = true
	}
	if len(drop) == 0 {
		return decode
	}
	keep := func(in []candidate) []candidate {
		out := in[:0:0]
		for _, c := range in {
			if !drop[c.instanceUUID] {
				out = append(out, c)
			}
		}
		return out
	}
	r.prefill = keep(r.prefill)
	return keep(decode)
}

// admit files a ready instance under its role: decode instances are returned
// for the ordinary candidate list, prefill ones kept here. An instance without
// a role of this mode serves neither.
func (r *pdRoute) admit(inst *store.Instance, c candidate) (candidate, bool) {
	switch inst.Role {
	case pdmode.RoleDecode:
		return c, true
	case pdmode.RolePrefill:
		record := planning.ReadScheduleRecord(inst.ScheduleExpl).PDRecord()
		c.kvHost, c.kvPort = record.Address, record.Port
		r.prefill = append(r.prefill, c)
	}
	return candidate{}, false
}

// handoffs counts, per prefill instance, the requests whose KV it holds for
// a decoder that has not started answering yet: from a successful prefill
// stage until the decoder's first byte, which it cannot send before it has
// pulled the KV. A draining prefill instance may stop once its count is 0.
type handoffs struct {
	mu     sync.Mutex
	counts map[string]int
}

func (h *handoffs) begin(uuid string) func() {
	h.mu.Lock()
	if h.counts == nil {
		h.counts = map[string]int{}
	}
	h.counts[uuid]++
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			if h.counts[uuid]--; h.counts[uuid] <= 0 {
				delete(h.counts, uuid)
			}
			h.mu.Unlock()
		})
	}
}

func (h *handoffs) count(uuid string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[uuid]
}

// InFlight is how much work an instance still holds: requests it is
// answering and, for a prefill instance, KV it holds for a decoder. The
// controller waits for 0 before removing an instance being drained.
func (p *Proxy) InFlight(instanceUUID string) int {
	return p.handoffs.count(instanceUUID) + p.balancer.inflight(instanceUUID)
}
