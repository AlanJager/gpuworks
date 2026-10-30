package automation

import (
	"fmt"
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

// Waiter is a deployment with requests held for its wake, or (Place) one
// whose next replica waits for room to be placed.
type Waiter struct {
	DeploymentID uint
	Policy       servicelevel.Policy
	// Since is when its oldest held request came, or its replica started
	// waiting.
	Since time.Time
	// Place says it has nothing asleep to wake: what blocks it is what keeps
	// its replica from being placed, and once that sleeps the controller
	// places it.
	Place bool
}

// Holder is an awake replica on cards a waiting deployment needs, and the
// deployment it serves.
type Holder struct {
	DeploymentID uint
	Name         string
	Policy       servicelevel.Policy
	// Instances are the replica's instances, put to sleep together.
	Instances []string
	// Starting says the replica is still starting: it has served nothing
	// yet. Only the replica's own instances say so; another replica of its
	// deployment starting or waiting to be placed does not.
	Starting bool
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
	// and active, with its policy; a deployment put back to sleep by an
	// operator is not.
	Wakeable(deploymentID uint) (policy servicelevel.Policy, ok bool)
	// Blocking is what keeps the deployment's waiting sleepers from waking.
	Blocking(deploymentID uint) (Blocking, error)
	// Activity is the deployment's traffic.
	Activity(deploymentID uint) Activity
	// Settling says the deployment is not serving yet: its replica waits to
	// be placed, or an instance of it is still starting. It is asked of a
	// deployment a holder gave its cards to (stillWakingFor); whether a
	// holder itself is starting is its own replica's (Holder.Starting).
	Settling(deploymentID uint) bool
}

// ResidencyActions is how the residency arbiter acts.
type ResidencyActions interface {
	// Yield puts a holder's replica to sleep for another deployment, cutting
	// its requests off when abort; it returns the level it chose (the
	// holder's sleep level preference, servicelevel.Policy.SleepLevel).
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
	// wakeTrigger is why the arbiter ordered a wake that waits for room:
	// the waiting-wakes pass records it when the wake lands.
	wakeTrigger map[uint]string
	// yieldedFor is whom each holder gave its cards to, and when: it does
	// not take them back while that one is still waking, or it would take the
	// room the yield made before the waiter could use it.
	yieldedFor map[uint]yielded
	// next is when the last look found the next yield due, zero when none
	// is (Evaluator.residencyDue).
	next time.Time
	// starting are holders last seen still starting: once one serves, its
	// turn and idle time count from then.
	starting map[uint]bool
}

type yielded struct {
	to uint
	at time.Time
}

// yieldGuard is how long a holder leaves the cards it gave up to the one it
// gave them to, at most, while that one wakes; startGuard, while that one
// is placed and starts, which loading a model can take minutes of.
const (
	yieldGuard = 2 * time.Minute
	startGuard = 10 * time.Minute
)

type drain struct {
	since time.Time
	// limit is how long requests may run on.
	limit     time.Duration
	instances []string
	params    map[string]any
}

// yieldPoll is how often the arbiter looks while a yield it ordered lands,
// so the waiter wakes as soon as the room is there.
const yieldPoll = 250 * time.Millisecond

// busyPoll is how often the arbiter looks again at a holder that yields
// once idle but is busy: no clock says when its requests end.
const busyPoll = 2 * time.Second

// startingPoll is how often the arbiter looks again at a holder still
// starting: nothing tells it when the engine is ready.
const startingPoll = 2 * time.Second

// wakeRepeat is how long after ordering a waiter's wake the arbiter leaves it
// alone.
const wakeRepeat = 30 * time.Second

// WithResidency turns the residency arbiter on.
func (e *Evaluator) WithResidency(source ResidencySource, actions ResidencyActions) *Evaluator {
	e.res = &residencyState{
		source: source, actions: actions, demand: make(chan struct{}, 1),
		awakeAt: map[uint]time.Time{}, preempted: map[uint]time.Time{}, draining: map[uint]drain{}, wokeAt: map[uint]time.Time{},
		yieldedFor: map[uint]yielded{}, wakeTrigger: map[uint]string{}, starting: map[uint]bool{},
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
	policy servicelevel.Policy
	since  time.Time
	resume bool
	place  bool
}

// residency wakes the deployments requests wait for, and makes room for
// them by putting holders to sleep when their yield policies say they must
// give way (servicelevel.Yields). It notes when the next yield falls due.
func (e *Evaluator) residency(now time.Time) {
	r := e.res
	if r == nil {
		return
	}
	r.next = time.Time{}
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
		queue = append(queue, waiting{id: w.DeploymentID, policy: w.Policy, since: w.Since, place: w.Place})
	}
	for id, since := range r.preempted {
		if listed[id] {
			continue
		}
		policy, ok := r.source.Wakeable(id)
		if !ok {
			delete(r.preempted, id)
			continue
		}
		if e.stillWakingFor(now, id) {
			continue
		}
		queue = append(queue, waiting{id: id, policy: policy, since: since, resume: true})
	}
	sort.SliceStable(queue, func(a, b int) bool {
		ra, rb := queue[a].policy.Rank(), queue[b].policy.Rank()
		if ra != rb {
			return ra > rb
		}
		return queue[a].since.Before(queue[b].since)
	})
	if len(queue) > 0 {
		e.log.Debug("automation: residency look", "queue", queueText(queue))
	}
	yielding := map[uint]bool{}
	for id := range r.draining {
		yielding[id] = true
	}
	for _, w := range queue {
		ordered := false
		if at, ok := r.wokeAt[w.id]; ok && now.Sub(at) < wakeRepeat {
			// Its wake is in flight; one that waits for room still has its
			// blockers looked at, as their yields fall due.
			if _, waits := r.wakeTrigger[w.id]; !waits {
				continue
			}
			ordered = true
		}
		// A holder that gave its cards to one still waking leaves them be,
		// whether it waits to resume or has requests of its own waiting.
		if !w.resume && e.stillWakingFor(now, w.id) {
			continue
		}
		e.serveWaiter(now, w, ordered, yielding)
	}
	for id, at := range r.wokeAt {
		if now.Sub(at) >= wakeRepeat {
			delete(r.wokeAt, id)
		}
	}
	// While a yield lands, look often: the waiter's wake waits on it, and a
	// nudge would add its settle on top of the sleep.
	for _, y := range r.yieldedFor {
		if now.Sub(y.at) < yieldGuard {
			if _, asleep := r.source.Wakeable(y.to); asleep {
				r.dueAt(now.Add(yieldPoll))
				break
			}
		}
	}
}

// serveWaiter wakes a waiter, or, when its wake waits for room, puts to
// sleep what blocks it once that must give way. ordered says its wake was
// ordered already and waits for room: only the blockers are looked at, as
// for a replica waiting to be placed.
func (e *Evaluator) serveWaiter(now time.Time, w waiting, ordered bool, yielding map[uint]bool) {
	if !ordered && !w.place && !e.orderWake(now, w) {
		return
	}
	e.makeRoom(now, w, yielding)
}

// orderWake wakes the waiter's sleepers that fit; it says whether some
// wait for room.
func (e *Evaluator) orderWake(now time.Time, w waiting) bool {
	r := e.res
	trigger := TriggerRequest
	if w.resume {
		trigger = TriggerResume
	}
	result, err := e.actions.Wake(w.id)
	if err != nil {
		e.log.Warn("automation: wake a deployment a request waits for", "deployment", w.id, "err", err)
		return false
	}
	r.wokeAt[w.id] = now
	if len(result.Woken) > 0 {
		e.actions.Record(w.id, EventAutoWake, map[string]any{"trigger": trigger, "instances": result.Woken})
		r.awakeAt[w.id] = now
		delete(r.preempted, w.id)
		return false
	}
	if len(result.Waiting) == 0 {
		return false
	}
	r.wakeTrigger[w.id] = trigger
	return true
}

// makeRoom puts to sleep the holders on the cards the waiter needs, all of
// them or none, once each must give way; else it notes when one falls due.
func (e *Evaluator) makeRoom(now time.Time, w waiting, yielding map[uint]bool) {
	r := e.res
	blocking, err := r.source.Blocking(w.id)
	if err != nil {
		e.log.Warn("automation: read what keeps a deployment asleep", "deployment", w.id, "err", err)
		return
	}
	if blocking.Blocked || len(blocking.Holders) == 0 {
		return
	}
	waiter := servicelevel.Waiter{Policy: w.policy}
	reasons := make([]servicelevel.Reason, len(blocking.Holders))
	for i, h := range blocking.Holders {
		if yielding[h.DeploymentID] || h.DeploymentID == w.id {
			return
		}
		// Put to sleep for a waiter already: the sleep has not landed, and
		// the cards it frees are that waiter's.
		if y, ok := r.yieldedFor[h.DeploymentID]; ok && now.Sub(y.at) < yieldGuard {
			return
		}
		// A holder still starting has served nothing and is asked nothing:
		// look again once it may have started.
		if h.Starting {
			r.starting[h.DeploymentID] = true
			r.dueAt(now.Add(startingPoll))
			return
		}
		state := e.holderState(now, h)
		reason, ok := servicelevel.Yields(state, waiter)
		if !ok {
			e.noteDue(now, state, waiter)
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
	if r.starting[h.DeploymentID] {
		delete(r.starting, h.DeploymentID)
		r.awakeAt[h.DeploymentID] = now
	}
	since := e.started
	if at, ok := r.awakeAt[h.DeploymentID]; ok && at.After(since) {
		since = at
	}
	out := servicelevel.Holder{Policy: h.Policy, AwakeFor: now.Sub(since)}
	act := r.source.Activity(h.DeploymentID)
	out.Busy = act.InFlight > 0 || act.Held > 0
	if !out.Busy {
		last := since
		if act.LastRequest.After(last) {
			last = act.LastRequest
		}
		out.IdleFor = now.Sub(last)
	}
	return out
}

// noteDue moves the next look up to when holder must give way to waiter, or
// to the next busy poll when it gives way once idle and is busy now.
func (e *Evaluator) noteDue(now time.Time, holder servicelevel.Holder, waiter servicelevel.Waiter) {
	after, ok := servicelevel.Due(holder, waiter)
	if !ok {
		if !holder.Busy || holder.Policy.IdleRelease() == 0 {
			return
		}
		after = busyPoll
	}
	e.res.dueAt(now.Add(after))
}

// dueAt moves the next look up to at.
func (r *residencyState) dueAt(at time.Time) {
	if r.next.IsZero() || at.Before(r.next) {
		r.next = at
	}
}

// residencyDue is how long until the arbiter must look again, ok false when
// nothing falls due (the tick still looks).
func (e *Evaluator) residencyDue() (time.Duration, bool) {
	if e.res == nil || e.res.next.IsZero() {
		return 0, false
	}
	after := e.res.next.Sub(e.now())
	if after < 0 {
		after = 0
	}
	return after, true
}

// yield puts a holder to sleep for a waiter. A holder idle for the reason has
// nothing running and sleeps at once. Otherwise its policy says what becomes
// of the requests still running (Policy.Drain): cut off at once, or new ones
// are held and those running get the time its policy gives them.
func (e *Evaluator) yield(now time.Time, h Holder, reason servicelevel.Reason, w waiting) {
	r := e.res
	params := map[string]any{"trigger": TriggerPreempt, "reason": string(reason), "for_deployment": w.id, "instances": h.Instances}
	abort, limit := h.Policy.Drain()
	if reason == servicelevel.ReasonIdle {
		e.sleepHolder(h.DeploymentID, h.Instances, false, params)
		return
	}
	if !abort && r.source.Activity(h.DeploymentID).InFlight > 0 {
		r.actions.Drain(h.DeploymentID, true)
		r.draining[h.DeploymentID] = drain{since: now, limit: limit, instances: h.Instances, params: params}
		r.dueAt(now.Add(min(limit, busyPoll)))
		return
	}
	e.sleepHolder(h.DeploymentID, h.Instances, abort, params)
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
	if to, ok := params["for_deployment"].(uint); ok {
		r.yieldedFor[id] = yielded{to: to, at: e.now()}
	}
	if policy, ok := r.source.Wakeable(id); ok && policy.Preemptible() {
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
			// Its requests may end sooner than the limit: look again soon.
			r.dueAt(d.since.Add(d.limit))
			r.dueAt(now.Add(busyPoll))
			continue
		}
		delete(r.draining, id)
		e.sleepHolder(id, d.instances, inFlight > 0, d.params)
		r.actions.Drain(id, false)
	}
}

// stillWakingFor says the deployment gave its cards to one that has not woken
// yet: it leaves them alone until that one is awake, or for yieldGuard.
func (e *Evaluator) stillWakingFor(now time.Time, id uint) bool {
	r := e.res
	y, ok := r.yieldedFor[id]
	if !ok {
		return false
	}
	if now.Sub(y.at) < yieldGuard {
		if _, asleep := r.source.Wakeable(y.to); asleep {
			return true
		}
	}
	if now.Sub(y.at) < startGuard && r.source.Settling(y.to) {
		return true
	}
	// The one it gave way to serves now: its turn, and its idle time,
	// count from here.
	if at, ok := r.awakeAt[y.to]; !ok || at.Before(y.at) {
		r.awakeAt[y.to] = now
	}
	delete(r.yieldedFor, id)
	return false
}

func queueText(queue []waiting) []string {
	out := make([]string, len(queue))
	for i, w := range queue {
		kind := "request"
		switch {
		case w.place:
			kind = "place"
		case w.resume:
			kind = "resume"
		}
		out[i] = fmt.Sprintf("%d:%s:%s", w.id, w.policy.Yield, kind)
	}
	return out
}
