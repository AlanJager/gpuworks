package proxy

// A replica about to give its cards up drains first: the gateway routes no
// new request to it while those in flight finish, and the deployment's other
// replicas go on serving. Only when every replica a request could go to
// drains is the request held, as for a deployment asleep, until the drain
// ends or the deployment wakes elsewhere.

// SetDraining stops (on) or resumes routing new requests to the deployment's
// instances: while they drain, a request goes to another replica, or is held
// as if the deployment slept when none is left.
func (p *Proxy) SetDraining(deploymentID uint, instances []string, on bool) {
	h := p.hold
	h.mu.Lock()
	next := make(map[string]bool, len(h.draining[deploymentID])+len(instances))
	for uuid := range h.draining[deploymentID] {
		next[uuid] = true
	}
	for _, uuid := range instances {
		if on {
			next[uuid] = true
		} else {
			delete(next, uuid)
		}
	}
	if len(next) == 0 {
		delete(h.draining, deploymentID)
	} else {
		h.draining[deploymentID] = next
	}
	h.mu.Unlock()
	if !on {
		h.signal()
	}
}

// drainingOf is the deployment's draining instances, nil when none drains.
// The set is never changed once handed out (SetDraining replaces it).
func (h *wakeHold) drainingOf(deploymentID uint) map[string]bool {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining[deploymentID]
}

// InstancesInFlight is how many requests the gateway is serving on the
// instances: what a drain waits for.
func (p *Proxy) InstancesInFlight(instances []string) int {
	n := 0
	for _, uuid := range instances {
		n += p.balancer.inflight(uuid)
	}
	return n
}

// withoutDraining is candidates less the draining ones; candidates itself
// when none drains, so a request with nothing draining copies nothing.
func withoutDraining(candidates []candidate, draining map[string]bool) []candidate {
	if len(draining) == 0 {
		return candidates
	}
	out := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if !draining[c.instanceUUID] {
			out = append(out, c)
		}
	}
	return out
}
