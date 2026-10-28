package automation

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Timeline event codes the automation records on a deployment. Their params
// always carry "trigger": "schedule" (with "schedule_id"), "idle" or
// "wake_waiting".
const (
	EventAutoSleep            = "AUTO_SLEEP"
	EventAutoWake             = "AUTO_WAKE"
	EventAutoScale            = "AUTO_SCALE"
	EventWakeWaiting          = "WAKE_WAITING"
	EventWakeWaitTimeout      = "WAKE_WAIT_TIMEOUT"
	EventScheduleActionFailed = "SCHEDULE_ACTION_FAILED"
	// EventYieldedForWake is on the deployment an operator's yielding wake
	// put to sleep: params name the deployment woken and the level.
	EventYieldedForWake = "YIELDED_FOR_WAKE"
)

// Triggers say why the automation acted; TriggerOperator is a wait an
// operator asked for.
const (
	TriggerOperator    = "operator"
	TriggerSchedule    = "schedule"
	TriggerIdle        = "idle"
	TriggerWakeWaiting = "wake_waiting"
)

// IdleDeployment is a deployment with idle sleep on.
type IdleDeployment struct {
	ID       uint
	AfterSec int
	Level    int
	// Awake says at least one of its instances serves (is not asleep and not
	// ordered to sleep): only then is there anything to put to sleep.
	Awake bool
}

// WaitingWake is a deployment's instances whose wake waits for room.
type WaitingWake struct {
	DeploymentID uint
	Instances    []string
	Since        time.Time
}

// WakeResult is what a wake order did: the instances it woke and those whose
// GPUs could not take them back, which now wait for room.
type WakeResult struct {
	Woken   []string
	Waiting []string
}

// Source is what the automation reads.
type Source interface {
	Schedules() ([]Schedule, error)
	IdleDeployments() ([]IdleDeployment, error)
	WaitingWakes() ([]WaitingWake, error)
	// LastRequestAt is when the gateway last saw a user request for the
	// deployment since the server started.
	LastRequestAt(deploymentID uint) (time.Time, bool)
}

// Actions is how the automation acts: the same orders an operator gives,
// each taking the lifecycle order lock.
type Actions interface {
	// Sleep puts the deployment's awake instances to sleep at level.
	Sleep(deploymentID uint, level int) error
	// Wake wakes the deployment's sleepers that fit and leaves the rest
	// waiting for room.
	Wake(deploymentID uint) (WakeResult, error)
	// WakeWaiting wakes the named waiting instances that fit now.
	WakeWaiting(deploymentID uint, instances []string) ([]string, error)
	// EndWaiting gives up the named instances' wait.
	EndWaiting(deploymentID uint, instances []string) error
	// ScaleReplicas sets a single-node deployment's replica count.
	ScaleReplicas(deploymentID uint, replicas int) error
	// MarkFired records that a schedule's occurrence at at was handled.
	MarkFired(scheduleID uint, at time.Time) error
	// Record adds an event to the deployment's timeline.
	Record(deploymentID uint, code string, params map[string]any)
}

// Defaults for the evaluator.
const (
	DefaultInterval = 15 * time.Second
	// DefaultCatchUp is how old an occurrence missed while the server was
	// down may be and still run when it starts.
	DefaultCatchUp = 12 * time.Hour
	// DefaultWakeWaitLimit is how long a wake waits for room before it gives
	// up.
	DefaultWakeWaitLimit = 30 * time.Minute
	// DefaultNudgeSettle gathers the nudges of a burst of changes - a fleet
	// redeploying sends one per instance write - into one look at the
	// waiting wakes.
	DefaultNudgeSettle = 2 * time.Second
)

// Evaluator carries out schedules, idle sleep and waiting wakes on a tick.
type Evaluator struct {
	source   Source
	actions  Actions
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	catchUp  time.Duration
	waitMax  time.Duration
	settle   time.Duration
	// started is when the evaluator began watching: a deployment is not idle
	// before it has been watched for its whole idle period, since the
	// gateway's request clock starts with the server.
	started time.Time
	// awakeSince is when each idle-sleep deployment was last seen go from
	// asleep to awake, so a deployment just woken is not put back to sleep
	// before anyone could use it.
	awakeSince map[uint]time.Time
	wasAwake   map[uint]bool
	wake       chan struct{}
}

// New returns an evaluator. log may be nil.
func New(source Source, actions Actions, log *slog.Logger) *Evaluator {
	if log == nil {
		log = slog.Default()
	}
	return &Evaluator{
		source: source, actions: actions, log: log, now: time.Now,
		interval: DefaultInterval, catchUp: DefaultCatchUp, waitMax: DefaultWakeWaitLimit,
		settle:     DefaultNudgeSettle,
		awakeSince: map[uint]time.Time{}, wasAwake: map[uint]bool{}, wake: make(chan struct{}, 1),
	}
}

// Nudge asks for a look at the waiting wakes soon, since one may fit once
// another instance sleeps or goes. Schedules and idle sleep keep to the tick:
// a change elsewhere does not make either due.
func (e *Evaluator) Nudge() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run evaluates on every tick, and looks at the waiting wakes once per
// settle period while nudges arrive, until ctx ends.
func (e *Evaluator) Run(ctx context.Context) {
	e.started = e.now()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	var settled <-chan time.Time
	e.Tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.Tick()
		case <-e.wake:
			if settled == nil {
				settled = time.After(e.settle)
			}
		case <-settled:
			settled = nil
			e.waitingWakes(e.now())
		}
	}
}

// Tick runs one evaluation: waiting wakes first (they free nothing and only
// take room that is there), then schedules, then idle sleep.
func (e *Evaluator) Tick() {
	now := e.now()
	if e.started.IsZero() {
		e.started = now
	}
	e.waitingWakes(now)
	e.schedules(now)
	e.idle(now)
}

func (e *Evaluator) waitingWakes(now time.Time) {
	waits, err := e.source.WaitingWakes()
	if err != nil {
		e.log.Warn("automation: read waiting wakes", "err", err)
		return
	}
	for _, w := range waits {
		if now.Sub(w.Since) >= e.waitMax {
			if err := e.actions.EndWaiting(w.DeploymentID, w.Instances); err != nil {
				e.log.Warn("automation: end a wake's wait", "deployment", w.DeploymentID, "err", err)
				continue
			}
			e.actions.Record(w.DeploymentID, EventWakeWaitTimeout, map[string]any{
				"trigger": TriggerWakeWaiting, "instances": w.Instances, "waited_sec": int(now.Sub(w.Since).Seconds()),
			})
			continue
		}
		woken, err := e.actions.WakeWaiting(w.DeploymentID, w.Instances)
		if err != nil {
			e.log.Warn("automation: wake waiting instances", "deployment", w.DeploymentID, "err", err)
			continue
		}
		if len(woken) > 0 {
			e.actions.Record(w.DeploymentID, EventAutoWake, map[string]any{"trigger": TriggerWakeWaiting, "instances": woken})
		}
	}
}

func (e *Evaluator) schedules(now time.Time) {
	schedules, err := e.source.Schedules()
	if err != nil {
		e.log.Warn("automation: read schedules", "err", err)
		return
	}
	type due struct {
		schedule Schedule
		at       time.Time
	}
	var run []due
	for _, s := range schedules {
		at, ok, stale := s.due(now, e.catchUp)
		if !ok {
			continue
		}
		if stale {
			if err := e.actions.MarkFired(s.ID, at); err != nil {
				e.log.Warn("automation: mark a stale occurrence", "schedule", s.ID, "err", err)
			}
			continue
		}
		run = append(run, due{s, at})
	}
	// Oldest first: when two orders of one deployment are both due (a server
	// that was down across both), the newer one is applied last and stands.
	sort.SliceStable(run, func(a, b int) bool { return run[a].at.Before(run[b].at) })
	for _, d := range run {
		e.fire(d.schedule, d.at)
	}
}

// fire carries out one occurrence and marks it handled whatever the outcome:
// a failed order is recorded and not retried, so an order that cannot be
// carried out (say, a deployment that cannot sleep) does not repeat every tick.
func (e *Evaluator) fire(s Schedule, at time.Time) {
	params := map[string]any{"trigger": TriggerSchedule, "schedule_id": s.ID, "scheduled_at": at.UTC().Format(time.RFC3339)}
	var err error
	switch s.Action {
	case "sleep":
		params["level"] = s.SleepLevel
		if err = e.actions.Sleep(s.DeploymentID, s.SleepLevel); err == nil {
			e.actions.Record(s.DeploymentID, EventAutoSleep, params)
		}
	case "wake":
		var result WakeResult
		if result, err = e.actions.Wake(s.DeploymentID); err == nil {
			if len(result.Woken) > 0 {
				woke := copyParams(params)
				woke["instances"] = result.Woken
				e.actions.Record(s.DeploymentID, EventAutoWake, woke)
			}
			if len(result.Waiting) > 0 {
				waiting := copyParams(params)
				waiting["instances"] = result.Waiting
				e.actions.Record(s.DeploymentID, EventWakeWaiting, waiting)
			}
		}
	case "replicas":
		params["replicas"] = s.Replicas
		if err = e.actions.ScaleReplicas(s.DeploymentID, s.Replicas); err == nil {
			e.actions.Record(s.DeploymentID, EventAutoScale, params)
		}
	default:
		return
	}
	if err != nil {
		failed := copyParams(params)
		failed["action"] = s.Action
		failed["reason"] = err.Error()
		if code, ok := codeOf(err); ok {
			failed["reason_code"] = code
		}
		e.actions.Record(s.DeploymentID, EventScheduleActionFailed, failed)
		e.log.Warn("automation: scheduled order failed", "schedule", s.ID, "deployment", s.DeploymentID, "action", s.Action, "err", err)
	}
	if err := e.actions.MarkFired(s.ID, at); err != nil {
		e.log.Warn("automation: mark an occurrence handled", "schedule", s.ID, "err", err)
	}
}

func (e *Evaluator) idle(now time.Time) {
	deployments, err := e.source.IdleDeployments()
	if err != nil {
		e.log.Warn("automation: read idle sleep deployments", "err", err)
		return
	}
	seen := map[uint]bool{}
	for _, d := range deployments {
		seen[d.ID] = true
		if !d.Awake {
			e.wasAwake[d.ID] = false
			continue
		}
		if !e.wasAwake[d.ID] {
			e.wasAwake[d.ID] = true
			e.awakeSince[d.ID] = now
		}
		last := e.started
		if since := e.awakeSince[d.ID]; since.After(last) {
			last = since
		}
		if at, ok := e.source.LastRequestAt(d.ID); ok && at.After(last) {
			last = at
		}
		idleFor := now.Sub(last)
		if idleFor < time.Duration(d.AfterSec)*time.Second {
			continue
		}
		params := map[string]any{"trigger": TriggerIdle, "level": d.Level, "idle_sec": int(idleFor.Seconds())}
		if err := e.actions.Sleep(d.ID, d.Level); err != nil {
			failed := copyParams(params)
			failed["action"] = "sleep"
			failed["reason"] = err.Error()
			if code, ok := codeOf(err); ok {
				failed["reason_code"] = code
			}
			e.actions.Record(d.ID, EventScheduleActionFailed, failed)
			// Not again until it has been idle a whole period more.
			e.awakeSince[d.ID] = now
			continue
		}
		e.actions.Record(d.ID, EventAutoSleep, params)
		e.wasAwake[d.ID] = false
	}
	for id := range e.wasAwake {
		if !seen[id] {
			delete(e.wasAwake, id)
			delete(e.awakeSince, id)
		}
	}
}

func copyParams(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// coded is an error with a stable product code (messages.ProductError).
type coded interface{ Code() string }

func codeOf(err error) (string, bool) {
	for e := err; e != nil; {
		if c, ok := e.(coded); ok && c.Code() != "" {
			return c.Code(), true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return "", false
		}
		e = u.Unwrap()
	}
	return "", false
}
