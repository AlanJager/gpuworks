package automation

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// Triggers of the residency arbiter's orders.
const (
	// TriggerRequest: a request through the gateway waits for the
	// deployment.
	TriggerRequest = "request"
	// TriggerResume: the deployment was put to sleep for another and takes
	// its cards back.
	TriggerResume = "resume"
	// TriggerPreempt: the deployment gave its cards to one waiting for them.
	TriggerPreempt = "preempt"
)

// Waiter is a deployment with requests held for its wake.
type Waiter struct {
	DeploymentID uint
	Level        string
	// Since is when its oldest held request came.
	Since time.Time
}

// Holder is an awake replica on cards a waiting deployment needs, and the
// deployment it serves.
type Holder struct {
	DeploymentID uint
	Name         string
	Level        string
	// Instances are the replica's instances, put to sleep together.
	Instances []string
}

// Blocking is what keeps a waiting deployment's sleepers from waking: the
// awake replicas on the cards they need, or an occupant nothing can put to
// sleep (a process outside the platform, a deployment that cannot sleep).
type Blocking struct {
	Holders []Holder
	Blocked bool
}

// Activity is a deployment's traffic as the gateway sees it.
type Activity struct {
	InFlight    int
	Held        int
	LastRequest time.Time
}

// ResidencySource is what the residency arbiter reads.
type ResidencySource interface {
	// Waiters are the deployments with a request held for their wake whose
	// sleep a request may end (not held by an operator or a schedule).
	Waiters() ([]Waiter, error)
	// Wakeable says the deployment is still asleep by the platform's choice
	// and active, with its level; a deployment put back to sleep by an
	// operator is not.
	Wakeable(deploymentID uint) (level string, ok bool)
	// Blocking is what keeps the deployment's waiting sleepers from waking.
	Blocking(deploymentID uint) (Blocking, error)
	// Activity is the deployment's traffic.
	Activity(deploymentID uint) Activity
}

// ResidencyActions is how the residency arbiter acts.
type ResidencyActions interface {
	// Yield puts a holder's replica to sleep for another deployment, cutting
	// its requests off when abort; it returns the level it chose.
	Yield(deploymentID uint, instances []string, abort bool) (int, error)
	// Drain stops (on) or resumes routing new requests to the deployment;
	// they are held as if it slept.
	Drain(deploymentID uint, on bool)
}

// DefaultDemandSettle gathers the demand of a burst of requests for sleeping
// deployments into one look.
const DefaultDemandSettle = 100 * time.Millisecond

// residencyState is the arbiter's memory. It is not persisted: after a
// restart a deployment put to sleep for another resumes only when a request
// comes for it.
type residencyState struct {
	source  ResidencySource
	actions ResidencyActions
	demand  chan struct{}
	// awakeAt is when the arbiter woke each deployment, the start of its
	// turn.
	awakeAt map[uint]time.Time
	// preempted are deployments put to sleep for another, since when: they
	// take their cards back once they may.
	preempted map[uint]time.Time
	// draining are holders whose requests run on before they sleep.
	draining map[uint]drain
	// wokeAt is when a wake was last ordered for a waiter, so a waiter whose
	// wake is in flight is not ordered again on every pass.
	wokeAt map[uint]time.Time
}

type drain struct {
	since     time.Time
	limit     time.Duration
	instances []string
	params    map[string]any
}

// wakeRepeat is how long after ordering a waiter's wake the arbiter leaves it
// alone.
const wakeRepeat = 30 * time.Second

// WithResidency turns the residency arbiter on.
func (e *Evaluator) WithResidency(source ResidencySource, actions ResidencyActions) *Evaluator {
	e.res = &residencyState{
		source: source, actions: actions, demand: make(chan struct{}, 1),
		awakeAt: map[uint]time.Time{}, preempted: map[uint]time.Time{}, draining: map[uint]drain{}, wokeAt: map[uint]time.Time{},
	}
	return e
}

// Demand says a deployment has a request held for its wake: the arbiter
// looks soon.
func (e *Evaluator) Demand(uint) {
	if e.res == nil {
		return
	}
	select {
	case e.res.demand <- struct{}{}:
	default:
	}
}

func (e *Evaluator) demandChan() <-chan struct{} {
	if e.res == nil {
		return nil
	}
	return e.res.demand
}

type waiting struct {
	id     uint
	level  string
	since  time.Time
	resume bool
}

// residency wakes the deployments requests wait for, and makes room for
// them by putting holders to sleep when the service levels say they must give
// way (servicelevel.Yields).
func (e *Evaluator) residency(now time.Time) {
	r := e.res
	if r == nil {
		return
	}
	e.finishDrains(now)
	waiters, err := r.source.Waiters()
	if err != nil {
		e.log.Warn("automation: read waiting requests", "err", err)
		return
	}
	var queue []waiting
	listed := map[uint]bool{}
	for _, w := range waiters {
		listed[w.DeploymentID] = true
		queue = append(queue, waiting{id: w.DeploymentID, level: w.Level, since: w.Since})
	}
	for id, since := range r.preempted {
		if listed[id] {
			continue
		}
		level, ok := r.source.Wakeable(id)
		if !ok {
			delete(r.preempted, id)
			continue
		}
		queue = append(queue, waiting{id: id, level: level, since: since, resume: true})
	}
	sort.SliceStable(queue, func(a, b int) bool {
		ra, rb := servicelevel.For(queue[a].level).Rank, servicelevel.For(queue[b].level).Rank
		if ra != rb {
			return ra > rb
		}
		return queue[a].since.Before(queue[b].since)
	})
	yielding := map[uint]bool{}
	for id := range r.draining {
		yielding[id] = true
	}
	for _, w := range queue {
		if at, ok := r.wokeAt[w.id]; ok && now.Sub(at) < wakeRepeat {
			continue
		}
		e.serveWaiter(now, w, yielding)
	}
	for id, at := range r.wokeAt {
		if now.Sub(at) >= wakeRepeat {
			delete(r.wokeAt, id)
		}
	}
}

func (e *Evaluator) serveWaiter(now time.Time, w waiting, yielding map[uint]bool) {
	r := e.res
	trigger := TriggerRequest
	if w.resume {
		trigger = TriggerResume
	}
	result, err := e.actions.Wake(w.id)
	if err != nil {
		e.log.Warn("automation: wake a deployment a request waits for", "deployment", w.id, "err", err)
		return
	}
	r.wokeAt[w.id] = now
	if len(result.Woken) > 0 {
		e.actions.Record(w.id, EventAutoWake, map[string]any{"trigger": trigger, "instances": result.Woken})
		r.awakeAt[w.id] = now
		delete(r.preempted, w.id)
		return
	}
	if len(result.Waiting) == 0 {
		return
	}
	blocking, err := r.source.Blocking(w.id)
	if err != nil {
		e.log.Warn("automation: read what keeps a deployment asleep", "deployment", w.id, "err", err)
		return
	}
	if blocking.Blocked || len(blocking.Holders) == 0 {
		return
	}
	waiter := servicelevel.Waiter{Level: w.level, Waited: now.Sub(w.since)}
	reasons := make([]servicelevel.Reason, len(blocking.Holders))
	for i, h := range blocking.Holders {
		if yielding[h.DeploymentID] || h.DeploymentID == w.id {
			return
		}
		reason, ok := servicelevel.Yields(e.holderState(now, h), waiter)
		if !ok {
			return
		}
		reasons[i] = reason
	}
	// Every holder gives way, or none does: putting some to sleep and not
	// the others would take service away without making room.
	for i, h := range blocking.Holders {
		yielding[h.DeploymentID] = true
		e.yield(now, h, reasons[i], w)
	}
	// The waiter's wake waits for room; the waiting-wakes pass finishes it
	// once the holders sleep.
}

// holderState is a holder as the yield rule reads it.
func (e *Evaluator) holderState(now time.Time, h Holder) servicelevel.Holder {
	r := e.res
	since := e.started
	if at, ok := r.awakeAt[h.DeploymentID]; ok && at.After(since) {
		since = at
	}
	out := servicelevel.Holder{Level: h.Level, AwakeFor: now.Sub(since)}
	act := r.source.Activity(h.DeploymentID)
	if act.InFlight == 0 && act.Held == 0 {
		last := since
		if act.LastRequest.After(last) {
			last = act.LastRequest
		}
		out.IdleFor = now.Sub(last)
	}
	return out
}

// yield puts a holder to sleep for a waiter. A level that lets requests run
// on (shared) first stops new ones and drains; offline is cut off at once.
func (e *Evaluator) yield(now time.Time, h Holder, reason servicelevel.Reason, w waiting) {
	r := e.res
	params := map[string]any{"trigger": TriggerPreempt, "reason": string(reason), "for_deployment": w.id, "instances": h.Instances}
	policy := servicelevel.For(h.Level)
	if policy.Drain > 0 && r.source.Activity(h.DeploymentID).InFlight > 0 {
		r.actions.Drain(h.DeploymentID, true)
		r.draining[h.DeploymentID] = drain{since: now, limit: policy.Drain, instances: h.Instances, params: params}
		return
	}
	e.sleepHolder(h.DeploymentID, h.Instances, policy.Drain == 0 && reason != servicelevel.ReasonIdle, params)
}

func (e *Evaluator) sleepHolder(id uint, instances []string, abort bool, params map[string]any) {
	r := e.res
	params["abort"] = abort
	level, err := r.actions.Yield(id, instances, abort)
	if err != nil {
		failed := copyParams(params)
		failed["action"] = "sleep"
		failed["reason"] = err.Error()
		if code, ok := codeOf(err); ok {
			failed["reason_code"] = code
		}
		e.actions.Record(id, EventScheduleActionFailed, failed)
		return
	}
	params["level"] = level
	e.actions.Record(id, EventAutoSleep, params)
	delete(r.awakeAt, id)
	if level, ok := r.source.Wakeable(id); ok && servicelevel.For(level).Preemptible {
		r.preempted[id] = e.now()
	}
}

// finishDrains puts each draining holder to sleep once its requests are done
// or its drain time is up, cutting off what still runs.
func (e *Evaluator) finishDrains(now time.Time) {
	r := e.res
	for id, d := range r.draining {
		inFlight := r.source.Activity(id).InFlight
		if inFlight > 0 && now.Sub(d.since) < d.limit {
			continue
		}
		delete(r.draining, id)
		e.sleepHolder(id, d.instances, inFlight > 0, d.params)
		r.actions.Drain(id, false)
	}
}
