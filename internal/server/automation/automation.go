package automation

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Timeline event codes the automation records on a deployment. Their params
// always carry "trigger": "schedule" (with "schedule_id"), "wake_waiting", or
// one of the residency arbiter's (TriggerRequest and the others).
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
	TriggerWakeWaiting = "wake_waiting"
)

// LevelAuto is the sleep level of an order that leaves the choice of L1 or L2
// to the platform: L1, and L2 where the host cannot hold the weights.
const LevelAuto = 0

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
	WaitingWakes() ([]WaitingWake, error)
}

// Actions is how the automation acts: the same orders an operator gives,
// each taking the lifecycle order lock.
type Actions interface {
	// Sleep puts the deployment's awake instances to sleep at level
	// (LevelAuto: the platform's choice) and returns the level it ordered. A
	// held sleep (a schedule's) is not woken by a request; one the platform
	// chose (auto sleep) is.
	Sleep(deploymentID uint, level int, held bool) (int, error)
	// Wake wakes the deployment's sleepers that fit and leaves the rest
	// waiting for room.
	Wake(deploymentID uint) (WakeResult, error)
	// WakeWaiting wakes the named waiting instances that fit now.
	WakeWaiting(deploymentID uint, instances []string) ([]string, error)
	// EndWaiting gives up the named instances' wait.
	EndWaiting(deploymentID uint, instances []string) error
	// ScaleReplicas sets the deployment's replica count; of a PD deployment,
	// the count of one role (role is empty for any other).
	ScaleReplicas(deploymentID uint, role string, replicas int) error
	// MarkFired records that a schedule's occurrence at at was handled.
	MarkFired(scheduleID uint, at time.Time) error
	// Record adds an event to the deployment's timeline.
	Record(deploymentID uint, code string, params map[string]any)
}

// Defaults for the evaluator.
const (
	DefaultInterval = 10 * time.Second
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

// Evaluator carries out schedules and waiting wakes on a tick, and the
// residency arbiter's yields when they fall due.
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
	wake    chan struct{}
	// res is the residency arbiter, nil when off (WithResidency).
	res *residencyState
}

// New returns an evaluator. log may be nil.
func New(source Source, actions Actions, log *slog.Logger) *Evaluator {
	if log == nil {
		log = slog.Default()
	}
	return &Evaluator{
		source: source, actions: actions, log: log, now: time.Now,
		interval: DefaultInterval, catchUp: DefaultCatchUp, waitMax: DefaultWakeWaitLimit,
		settle: DefaultNudgeSettle, wake: make(chan struct{}, 1),
	}
}

// Nudge asks for a look at the waiting wakes soon, since one may fit once
// another instance sleeps or goes. Schedules keep to the tick: a change
// elsewhere does not make one due.
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
	var settled, demanded <-chan time.Time
	// due fires when the residency arbiter's next yield falls due (an idle
	// time or a turn ending, a drain running out): one timer, re-armed after
	// every look.
	due := time.NewTimer(time.Hour)
	defer due.Stop()
	rearm := func() {
		if !due.Stop() {
			select {
			case <-due.C:
			default:
			}
		}
		if after, ok := e.residencyDue(); ok {
			due.Reset(after)
		}
	}
	e.Tick()
	rearm()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.Tick()
			rearm()
		case <-due.C:
			e.due(e.now())
			rearm()
		case <-e.wake:
			if settled == nil {
				settled = time.After(e.settle)
			}
		case <-e.demandChan():
			if demanded == nil {
				demanded = time.After(DefaultDemandSettle)
			}
		case <-demanded:
			demanded = nil
			e.residency(e.now())
			rearm()
		case <-settled:
			settled = nil
			now := e.now()
			e.waitingWakes(now)
			e.residency(now)
			rearm()
		}
	}
}

// Tick runs one evaluation: waiting wakes first (they free nothing and only
// take room that is there), then the residency arbiter, then schedules.
func (e *Evaluator) Tick() {
	now := e.now()
	if e.started.IsZero() {
		e.started = now
	}
	e.waitingWakes(now)
	e.residency(now)
	e.schedules(now)
}

func (e *Evaluator) waitingWakes(now time.Time) { e.waitingWakesOf(now, nil) }

// waitingWakesOf looks at the waiting wakes of the deployments only names,
// or at every one when only is nil.
func (e *Evaluator) waitingWakesOf(now time.Time, only map[uint]bool) {
	waits, err := e.source.WaitingWakes()
	if err != nil {
		e.log.Warn("automation: read waiting wakes", "err", err)
		return
	}
	for _, w := range waits {
		if only != nil && !only[w.DeploymentID] {
			continue
		}
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
			trigger := TriggerWakeWaiting
			if e.res != nil {
				if why, ok := e.res.wakeTrigger[w.DeploymentID]; ok {
					trigger = why
					delete(e.res.wakeTrigger, w.DeploymentID)
				}
				e.res.awakeAt[w.DeploymentID] = now
				delete(e.res.preempted, w.DeploymentID)
			}
			e.actions.Record(w.DeploymentID, EventAutoWake, map[string]any{"trigger": trigger, "instances": woken})
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
		if _, err = e.actions.Sleep(s.DeploymentID, s.SleepLevel, true); err == nil {
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
		if s.Role != "" {
			params["role"] = s.Role
		}
		if err = e.actions.ScaleReplicas(s.DeploymentID, s.Role, s.Replicas); err == nil {
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
