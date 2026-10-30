package proxy

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bounds on the requests the gateway holds while their deployments wake.
const (
	// wakeHoldPerDeployment is how many requests one deployment may have
	// held; more are refused at once rather than queued without end.
	wakeHoldPerDeployment = 256
	// wakeHoldTotal bounds the held requests of all deployments together.
	wakeHoldTotal = 4096
	// wakeHoldRecheck is how often a held request looks again even when no
	// route change has signalled it, so a missed signal costs at most this.
	wakeHoldRecheck = time.Second
)

// wakeHold holds the requests for deployments the platform put to sleep
// while they wake (docs/explanation/service-levels.md). A request waits until
// its deployment has an awake replica, its policy's HoldMax passes, or the
// client goes; every route table rebuild signals the waiters to look again.
type wakeHold struct {
	mu      sync.Mutex
	held    map[uint]map[*heldRequest]struct{}
	total   int
	changed chan struct{}
	// demand is told a deployment has a request waiting, so it is woken.
	demand func(deploymentID uint)
	now    func() time.Time
	// holdMax is how long a request for the deployment waits: its policy's
	// hold_max_sec.
	holdMax func(dep *store.Deployment) time.Duration
	// draining are deployments about to give their cards up: new requests
	// for them are held as if they slept, while those in flight finish.
	draining map[uint]bool
}

type heldRequest struct{ since time.Time }

func newWakeHold() *wakeHold {
	return &wakeHold{
		held: map[uint]map[*heldRequest]struct{}{}, changed: make(chan struct{}), now: time.Now, draining: map[uint]bool{},
		holdMax: func(dep *store.Deployment) time.Duration { return dep.Policy().HoldMax() },
	}
}

// wakesOnRequest says a request for dep should be held while it wakes
// (store.Deployment.WakesOnRequest).
func wakesOnRequest(dep *store.Deployment) bool { return dep.WakesOnRequest() }

// signal wakes every held request to look at the routes again.
func (h *wakeHold) signal() {
	h.mu.Lock()
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

// enter registers a held request, false when the bounds are full.
func (h *wakeHold) enter(deploymentID uint) (*heldRequest, <-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reqs := h.held[deploymentID]
	if len(reqs) >= wakeHoldPerDeployment || h.total >= wakeHoldTotal {
		return nil, nil, false
	}
	if reqs == nil {
		reqs = map[*heldRequest]struct{}{}
		h.held[deploymentID] = reqs
	}
	req := &heldRequest{since: h.now()}
	reqs[req] = struct{}{}
	h.total++
	return req, h.changed, true
}

func (h *wakeHold) next() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *wakeHold) leave(deploymentID uint, req *heldRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reqs := h.held[deploymentID]
	if _, ok := reqs[req]; !ok {
		return
	}
	delete(reqs, req)
	h.total--
	if len(reqs) == 0 {
		delete(h.held, deploymentID)
	}
}

// Held is how many requests the deployment has held and since when the
// oldest has waited.
func (h *wakeHold) Held(deploymentID uint) (int, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest time.Time
	for req := range h.held[deploymentID] {
		if oldest.IsZero() || req.since.Before(oldest) {
			oldest = req.since
		}
	}
	return len(h.held[deploymentID]), oldest
}

// HeldDeployments are the deployments with a request held.
func (h *wakeHold) HeldDeployments() []uint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]uint, 0, len(h.held))
	for id := range h.held {
		out = append(out, id)
	}
	return out
}

// SetWakeDemand sets what is told a deployment has a request held for its
// wake. Without one, requests for a sleeping deployment are refused as before.
func (p *Proxy) SetWakeDemand(fn func(deploymentID uint)) {
	p.hold.mu.Lock()
	p.hold.demand = fn
	p.hold.mu.Unlock()
}

// HeldRequests is how many requests the gateway holds for the deployment's
// wake and since when the oldest has waited.
func (p *Proxy) HeldRequests(deploymentID uint) (int, time.Time) {
	return p.hold.Held(deploymentID)
}

// SetDraining stops (on) or resumes routing new requests to the deployment:
// while it drains they are held as if it slept, so the requests in flight can
// finish before it gives its cards up.
func (p *Proxy) SetDraining(deploymentID uint, on bool) {
	p.hold.mu.Lock()
	if on {
		p.hold.draining[deploymentID] = true
	} else {
		delete(p.hold.draining, deploymentID)
	}
	p.hold.mu.Unlock()
	if !on {
		p.hold.signal()
	}
}

func (h *wakeHold) isDraining(deploymentID uint) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining[deploymentID]
}

// HeldDeployments are the deployments with a request held for their wake.
func (p *Proxy) HeldDeployments() []uint {
	return p.hold.HeldDeployments()
}

// holdForWake holds a request for a sleeping deployment until the deployment
// can serve it, then selects again. It returns the selection's result; a
// request still not served when its time is up gets the asleep refusal with
// a Retry-After.
func (p *Proxy) holdForWake(ctx context.Context, w http.ResponseWriter, modelName, affinity string, dep *store.Deployment) (*store.Deployment, target, int, string, bool) {
	refuse := func() (*store.Deployment, target, int, string, bool) {
		return dep, target{}, http.StatusServiceUnavailable, asleepMessage(dep), true
	}
	if p.hold == nil {
		return refuse()
	}
	p.hold.mu.Lock()
	demand := p.hold.demand
	p.hold.mu.Unlock()
	if demand == nil || !wakesOnRequest(dep) {
		return refuse()
	}
	req, changed, ok := p.hold.enter(dep.ID)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wakeHoldRecheck.Seconds())*30))
		return refuse()
	}
	defer p.leaveHold(dep.ID, req)
	demand(dep.ID)
	hold := p.hold.holdMax(dep)
	deadline := time.NewTimer(hold)
	defer deadline.Stop()
	recheck := time.NewTicker(wakeHoldRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return dep, target{}, http.StatusServiceUnavailable, "the request ended while the model woke", true
		case <-deadline.C:
			w.Header().Set("Retry-After", strconv.Itoa(max(int(hold.Seconds()), 1)))
			return refuse()
		case <-changed:
			changed = p.hold.next()
		case <-recheck.C:
		}
		got, tgt, status, msg, asleep := p.selectDeploymentInstanceWithKey(modelName, affinity)
		if status == 0 || !asleep {
			return got, tgt, status, msg, asleep
		}
		if got != nil && got.ID == dep.ID && !wakesOnRequest(got) {
			// An operator or a schedule put it to sleep meanwhile.
			return got, tgt, status, msg, asleep
		}
	}
}
