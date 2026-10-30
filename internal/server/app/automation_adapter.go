package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// automationEventKind is the timeline kind of what the automation does.
const automationEventKind = store.EventKindAutomation

// automationAdapter backs the scheduling automation with the store, the
// ledger, the gateway's request clock and the host memory the agents report.
// Every order takes the lifecycle order lock, as an operator's does.
type automationAdapter struct {
	store   *store.Store
	acct    *proxy.Accountant
	metrics tsdb.Store
	scale   func(deploymentID uint, role string, replicas int) error
	// waits are the replicas waiting to be placed (the controller's): a wake
	// does not take the cards one of at least its rank waits for.
	waits *placewait.Registry
	log   *slog.Logger
	now   func() time.Time
}

// runAutomation carries out schedules, auto sleep, waiting wakes and the
// residency arbiter until ctx ends. A change to an instance or a deployment
// nudges it, since a waiting wake may fit once another instance sleeps or
// goes; a request the gateway holds for a sleeping deployment asks it to wake
// that one.
func runAutomation(ctx context.Context, st *store.Store, px *proxy.Proxy, acct *proxy.Accountant, metrics tsdb.Store, scale func(deploymentID uint, role string, replicas int) error, waits *placewait.Registry, log *slog.Logger) {
	adapter := &automationAdapter{store: st, acct: acct, metrics: metrics, scale: scale, waits: waits, log: log, now: time.Now}
	evaluator := automation.New(adapter, adapter, log)
	if px != nil {
		residency := &residencyAdapter{store: st, proxy: px, acct: acct, host: adapter.hostMemory, waits: waits, now: time.Now}
		evaluator.WithResidency(residency, residency)
		if waits != nil {
			waits.OnWait(evaluator.Demand)
		}
		px.SetWakeDemand(evaluator.Demand)
	}
	if st.Bus != nil {
		sub := st.Bus.Subscribe(64, "instance", "deployment")
		go func() {
			defer sub.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case <-sub.C():
					evaluator.Nudge()
				}
			}
		}()
	}
	evaluator.Run(ctx)
}

// Schedules are the enabled schedules of active deployments. One whose time
// zone the server does not know is skipped and logged rather than run in the
// wrong zone.
func (a *automationAdapter) Schedules() ([]automation.Schedule, error) {
	rows, err := a.store.EnabledLifecycleSchedules()
	if err != nil {
		return nil, err
	}
	out := make([]automation.Schedule, 0, len(rows))
	for _, row := range rows {
		loc, err := scheduleLocation(row.Timezone)
		if err != nil {
			a.log.Warn("automation: schedule time zone unknown; skipped", "schedule", row.ID, "timezone", row.Timezone)
			continue
		}
		s := automation.Schedule{
			ID: row.ID, DeploymentID: row.DeploymentID, Action: row.Action, Weekdays: row.Weekdays, Minute: row.Minute,
			Location: loc, SleepLevel: row.SleepLevel, Replicas: row.Replicas, Role: row.Role,
		}
		if row.LastFiredAt != nil {
			s.LastFiredAt = *row.LastFiredAt
		}
		out = append(out, s)
	}
	return out, nil
}

// scheduleLocation is a schedule's time zone: its IANA name, or the server's
// own zone when it names none.
func scheduleLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	return time.LoadLocation(name)
}

// WaitingWakes groups the instances whose wake waits by deployment, each
// group's wait starting at its oldest.
func (a *automationAdapter) WaitingWakes() ([]automation.WaitingWake, error) {
	insts, err := a.store.InstancesWakeWaiting()
	if err != nil {
		return nil, err
	}
	var out []automation.WaitingWake
	at := map[uint]int{}
	for _, in := range insts {
		index, ok := at[in.DeploymentID]
		if !ok {
			at[in.DeploymentID] = len(out)
			out = append(out, automation.WaitingWake{DeploymentID: in.DeploymentID, Since: *in.WakeWaitingSince})
			index = len(out) - 1
		}
		out[index].Instances = append(out[index].Instances, in.UUID)
		if in.WakeWaitingSince.Before(out[index].Since) {
			out[index].Since = *in.WakeWaitingSince
		}
	}
	return out, nil
}

// sleepLevelOf is the sleep level order of a policy's preference.
func sleepLevelOf(pref string) int {
	switch pref {
	case servicelevel.LevelL1:
		return proto.SleepLevelL1
	case servicelevel.LevelL2:
		return proto.SleepLevelL2
	}
	return automation.LevelAuto
}

// Sleep orders a deployment to sleep as an operator's deployment-wide order
// does: refused when the deployment cannot sleep, or at L1 when a host
// cannot hold the weights. At the auto level it sleeps at L1, or at L2 where
// the host cannot hold the weights; it returns the level ordered.
func (a *automationAdapter) Sleep(deploymentID uint, level int, held bool) (int, error) {
	defer planning.LockLifecycle()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return 0, err
	}
	if code := planning.SleepEligibility(dep); code != "" {
		return 0, messages.NewProductError(code, "the deployment cannot sleep", map[string]any{"engine": dep.Engine})
	}
	auto := level == automation.LevelAuto
	if auto {
		level = proto.SleepLevelL1
	}
	if level == proto.SleepLevelL1 {
		insts, err := a.store.InstancesForDeployment(dep.ID)
		if err != nil {
			return 0, err
		}
		if refusal := planning.L1HostMemoryRefusal(a.store, dep, planning.Sleepers(insts), a.hostMemory); refusal != nil {
			if !auto {
				return 0, messages.NewProductError(refusal.Code, "the host memory cannot hold the weights an L1 sleep moves there", refusal.Params)
			}
			level = proto.SleepLevelL2
		}
	}
	return level, a.store.OrderDeploymentSleepHeld(dep.ID, level, proto.SleepModeWait, held)
}

// Wake wakes the deployment's sleepers that fit and leaves the rest waiting
// for room.
func (a *automationAdapter) Wake(deploymentID uint) (automation.WakeResult, error) {
	defer planning.LockLifecycle()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return automation.WakeResult{}, err
	}
	if code := planning.SleepEligibility(dep); code != "" {
		return automation.WakeResult{}, messages.NewProductError(code, "the deployment cannot sleep or wake", map[string]any{"engine": dep.Engine})
	}
	insts, err := a.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return automation.WakeResult{}, err
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return automation.WakeResult{}, err
	}
	sleepers, held := a.notForPlacement(dep, planning.Sleepers(insts))
	var woken []store.Instance
	var refused []planning.WakeRefusal
	if len(sleepers) > 0 {
		woken, refused, err = planning.OrderWake(a.store, dep, match, sleepers, true, len(held) == 0)
		if err != nil {
			return automation.WakeResult{}, err
		}
	}
	result := automation.WakeResult{Woken: uuidList(woken), Waiting: uuidList(held)}
	for _, r := range refused {
		result.Waiting = append(result.Waiting, uuidList(r.Instances)...)
	}
	if err := a.store.MarkWakeWaiting(dep.ID, result.Waiting, a.now()); err != nil {
		return result, err
	}
	return result, nil
}

// WakeWaiting wakes the named waiting instances whose GPUs can take them back
// now. An instance no longer waiting asleep (removed, rebuilt, woken or slept
// again by an operator) drops out.
func (a *automationAdapter) WakeWaiting(deploymentID uint, uuids []string) ([]string, error) {
	defer planning.LockLifecycle()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	insts, err := a.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, uuid := range uuids {
		want[uuid] = true
	}
	var named, gone []string
	var waiting []store.Instance
	for _, in := range insts {
		if !want[in.UUID] {
			continue
		}
		named = append(named, in.UUID)
		if !store.HoldsSleepIntent(&in) || planning.InstanceSleepIntent(&in) == 0 {
			gone = append(gone, in.UUID)
			continue
		}
		waiting = append(waiting, in)
	}
	if len(gone) > 0 {
		if err := a.store.EndWakeWaiting(dep.ID, gone); err != nil {
			return nil, err
		}
	}
	if len(waiting) == 0 {
		return nil, nil
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return nil, err
	}
	waiting, _ = a.notForPlacement(dep, waiting)
	if len(waiting) == 0 {
		return nil, nil
	}
	woken, _, err := planning.OrderWake(a.store, dep, match, waiting, true, false)
	if err != nil {
		return nil, err
	}
	return uuidList(woken), nil
}

// notForPlacement splits the sleepers of dep into those a wake may take
// cards for and those it leaves asleep: the ones on a card a replica waits
// to be placed on, of a deployment that ranks at least as high as dep (the
// cards were given up for it). The placement waits name the instances they
// wait for; their cards are the ones taken.
func (a *automationAdapter) notForPlacement(dep *store.Deployment, sleepers []store.Instance) (free, held []store.Instance) {
	if a.waits == nil {
		return sleepers, nil
	}
	waits := a.waits.Waiting(a.now())
	if len(waits) == 0 {
		return sleepers, nil
	}
	var holders []store.Instance
	rank := dep.Policy().Rank()
	for _, w := range waits {
		if w.DeploymentID == dep.ID {
			continue
		}
		waiter, err := a.store.GetDeployment(w.DeploymentID)
		if err != nil || waiter.Policy().Rank() < rank {
			continue
		}
		for _, uuid := range w.Instances {
			if in, err := a.store.InstanceByUUID(uuid); err == nil {
				holders = append(holders, *in)
			}
		}
	}
	taken := planning.InstanceCards(holders)
	for _, in := range sleepers {
		out := false
		for card := range planning.InstanceCards([]store.Instance{in}) {
			if taken[card] {
				out = true
				break
			}
		}
		if out {
			held = append(held, in)
		} else {
			free = append(free, in)
		}
	}
	return free, held
}

func (a *automationAdapter) EndWaiting(deploymentID uint, uuids []string) error {
	return a.store.EndWakeWaiting(deploymentID, uuids)
}

// ScaleReplicas sets the deployment's replica count through the API's own
// path (api.ReplicaScaler): a multi-node deployment the platform places gets
// its nodes picked as an operator's request would, a PD deployment's role is
// scaled through the PD update, and a refusal (a deployment placed by hand,
// no room for a replica, a role below its minimum) comes back as the product
// error the failure event records.
func (a *automationAdapter) ScaleReplicas(deploymentID uint, role string, replicas int) error {
	return a.scale(deploymentID, role, replicas)
}

func (a *automationAdapter) MarkFired(scheduleID uint, at time.Time) error {
	return a.store.MarkLifecycleScheduleFired(scheduleID, at)
}

func (a *automationAdapter) Record(deploymentID uint, code string, params map[string]any) {
	a.store.AddDeploymentEventCode(deploymentID, automationEventKind, code, params)
}

// hostMemory is a node's host memory as its agent reported it in the last two
// minutes.
func (a *automationAdapter) hostMemory(nodeUUID string) (available, total int64, ok bool) {
	if a.metrics == nil {
		return 0, 0, false
	}
	since := a.now().Add(-2 * time.Minute).UnixMilli()
	return planning.HostMemoryFromMetrics(func(metric, node string) (float64, bool) {
		rows, err := a.metrics.QueryLatest(metric, map[string]string{"node": node}, since)
		if err != nil || len(rows) == 0 {
			return 0, false
		}
		return rows[0].Value, true
	})(nodeUUID)
}

func uuidList(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	return out
}
