package proxy

import (
	"sync"
	"sync/atomic"
)

// An agent upgrade restarts the agent a node's instances are reached through,
// not their engines. Before it does, it asks the gateway to send the
// deployment's new requests to its other replicas while those in flight on
// the node finish (Avoid).
//
// Avoiding is a preference, never a refusal. Unlike a drain (drain.go), which
// holds a request as if the deployment slept when every replica drains, an
// avoided instance still takes a request when it is all that can: when the
// deployment's other replicas are asleep, on offline nodes, draining, or gone.
// The gateway judges that per request from its own view, so an upgrade never
// answers 503, nor holds a request, for a model that is awake on the node it
// is upgrading; the restart cuts what still runs there, as it did before.
//
// Single-node deployments only: a multi-node replica is spread over nodes, so
// there is no "other replica" a request could move to inside one, and PD
// deployments pick their prefill instances by their own rules.

// avoidSet is the instances an agent upgrade avoids, by deployment. Each
// Avoid call holds its instances until its release; an instance avoided twice
// is preferred against until both are released. The whole view is replaced on
// each change and read without a lock, so a request that avoids nothing pays
// one atomic load. The zero value is ready.
type avoidSet struct {
	mu     sync.Mutex // serializes writers
	counts map[uint]map[string]int
	view   atomic.Pointer[map[uint]map[string]bool]
}

// Avoid prefers the deployment's other replicas over instances for new
// requests until release is called. release is safe to call more than once.
func (p *Proxy) Avoid(deploymentID uint, instances []string) (release func()) {
	p.avoid.add(deploymentID, instances, 1)
	var once sync.Once
	return func() { once.Do(func() { p.avoid.add(deploymentID, instances, -1) }) }
}

func (a *avoidSet) add(deploymentID uint, instances []string, delta int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.counts == nil {
		a.counts = map[uint]map[string]int{}
	}
	counts := a.counts[deploymentID]
	if counts == nil {
		counts = map[string]int{}
	}
	for _, uuid := range instances {
		if counts[uuid] += delta; counts[uuid] <= 0 {
			delete(counts, uuid)
		}
	}
	if len(counts) == 0 {
		delete(a.counts, deploymentID)
	} else {
		a.counts[deploymentID] = counts
	}
	view := make(map[uint]map[string]bool, len(a.counts))
	for dep, held := range a.counts {
		set := make(map[string]bool, len(held))
		for uuid := range held {
			set[uuid] = true
		}
		view[dep] = set
	}
	a.view.Store(&view)
}

// of is the deployment's avoided instances, nil when none is avoided. The set
// is never changed once handed out.
func (a *avoidSet) of(deploymentID uint) map[string]bool {
	view := a.view.Load()
	if view == nil {
		return nil
	}
	return (*view)[deploymentID]
}

// pickAvoiding leases a replica of candidates preferring those not avoided:
// only when none of them can take the request well (none on an online node,
// none out of a failure cool-down, none awake, none with a free run slot) does
// it lease among all of them, so an avoided replica serves rather than the
// request being refused or sent to a replica known to be failing. The attempt
// keeps every candidate, and its retries prefer the same way (avoidedFirst),
// so a retry after the preferred replica failed can still reach the avoided
// one.
func (p *Proxy) pickAvoiding(candidates []candidate, avoided map[string]bool, policy, affinity string, fresh bool, adm admission) (*lease, bool) {
	if preferred := p.preferOthers(candidates, avoided); len(preferred) < len(candidates) {
		if l, _ := p.pickAwake(preferred, policy, affinity, fresh, adm); l != nil {
			return l, false
		}
	}
	return p.pickAwake(candidates, policy, affinity, fresh, adm)
}

// preferOthers is candidates less the avoided ones, when one of the rest can
// take a request well: on an online node and out of a failure cool-down (a
// cooling replica is what the balancer falls back to only when nothing else is
// left, and the avoided ones are left). candidates itself otherwise: nothing
// avoided, or nothing else to send the request to.
func (p *Proxy) preferOthers(candidates []candidate, avoided map[string]bool) []candidate {
	if len(avoided) == 0 {
		return candidates
	}
	others := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if !avoided[c.instanceUUID] {
			others = append(others, c)
		}
	}
	if len(others) == len(candidates) {
		return candidates
	}
	if !p.balancer.anyServing(others, p.hub.Online) {
		return candidates
	}
	return others
}

// ServedElsewhere reports whether the gateway would route the deployment's
// requests to a replica other than instances right now: one awake, on an
// online node and not draining. An upgrade waits for a node's requests to
// finish only then; otherwise the instances keep serving to the restart, as
// they did before an upgrade waited at all. It reads the route snapshot, and
// answers no for a deployment the snapshot does not hold, a multi-node one or
// a PD one.
func (p *Proxy) ServedElsewhere(deploymentID uint, instances []string) bool {
	if p.routes == nil {
		return false
	}
	rd := p.routes.deployment(deploymentID)
	if rd == nil || rd.nnodes > 1 || rd.pd != nil {
		return false
	}
	skip := make(map[string]bool, len(instances))
	for _, uuid := range instances {
		skip[uuid] = true
	}
	draining := p.hold.drainingOf(deploymentID)
	for _, c := range rd.candidates {
		if !skip[c.instanceUUID] && !draining[c.instanceUUID] && p.hub.Online(c.nodeUUID) {
			return true
		}
	}
	return false
}
