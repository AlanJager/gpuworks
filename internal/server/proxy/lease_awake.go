package proxy

import (
	"maps"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// pickBy leases a candidate by the deployment's routing policy, passing over
// the excluded ones.
func (b *balancer) pickBy(policy string, candidates []candidate, affinity string, excluded map[string]bool, online func(string) bool) *lease {
	switch policy {
	case deploy.Failover:
		return b.pickFailover(candidates, excluded, online)
	case deploy.PrefixAffinity:
		return b.pickAffinity(candidates, affinity, excluded, online)
	default:
		return b.pick(candidates, excluded, online)
	}
}

// leaseUntil leases with pick until accept takes the lease, giving back and
// excluding each one it turns down; nil when none is left. excluded is not
// modified.
func leaseUntil(pick func(excluded map[string]bool) *lease, excluded map[string]bool, accept func(*lease) bool) *lease {
	skip := maps.Clone(excluded)
	for {
		l := pick(skip)
		if l == nil || accept(l) {
			return l
		}
		l.release(outcomeCanceled)
		if skip == nil {
			skip = map[string]bool{}
		}
		skip[l.c.instanceUUID] = true
	}
}

// awake reports whether a leased instance may still take the request: not
// ordered to sleep since its candidate was read. A sleep order takes effect
// for routing at once, before the engine reports the new level, and a route
// snapshot is refreshed only asynchronously; this is one keyed read of the
// leased instance.
func (p *Proxy) awake(l *lease) bool {
	in, err := p.store.InstanceByUUID(l.c.instanceUUID)
	return err != nil || !planning.InstanceAsleep(in)
}

// pickAwake leases an awake candidate by the routing policy. Candidates built
// from rows read for this request (fresh) are taken as read; those from the
// route snapshot are checked, and when none passes the caller falls back to
// reading the rows, which tells asleep from not ready.
func (p *Proxy) pickAwake(candidates []candidate, policy, affinity string, fresh bool) *lease {
	accept := func(*lease) bool { return true }
	if !fresh {
		accept = p.awake
	}
	return leaseUntil(func(excluded map[string]bool) *lease {
		return p.balancer.pickBy(policy, candidates, affinity, excluded, p.hub.Online)
	}, nil, accept)
}
