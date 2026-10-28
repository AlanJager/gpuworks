package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
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
	log     *slog.Logger
	now     func() time.Time
}

// runAutomation carries out schedules, idle sleep and waiting wakes until ctx
// ends. A change to an instance or a deployment nudges it, since a waiting
// wake may fit once another instance sleeps or goes.
func runAutomation(ctx context.Context, st *store.Store, acct *proxy.Accountant, metrics tsdb.Store, log *slog.Logger) {
	adapter := &automationAdapter{store: st, acct: acct, metrics: metrics, log: log, now: time.Now}
	evaluator := automation.New(adapter, adapter, log)
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
			Location: loc, SleepLevel: row.SleepLevel, Replicas: row.Replicas,
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

// IdleDeployments are the deployments with idle sleep on, each saying whether
// any of its replicas serves.
func (a *automationAdapter) IdleDeployments() ([]automation.IdleDeployment, error) {
	deps, err := a.store.IdleSleepDeployments()
	if err != nil {
		return nil, err
	}
	out := make([]automation.IdleDeployment, 0, len(deps))
	for _, dep := range deps {
		insts, err := a.store.InstancesForDeployment(dep.ID)
		if err != nil {
			return nil, err
		}
		awake := false
		for i := range insts {
			in := &insts[i]
			if !in.Headless && in.State == proto.InstRunning && !planning.InstanceAsleep(in) {
				awake = true
				break
			}
		}
		out = append(out, automation.IdleDeployment{ID: dep.ID, AfterSec: dep.IdleSleepAfterSec, Level: dep.IdleSleepLevel, Awake: awake})
	}
	return out, nil
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

func (a *automationAdapter) LastRequestAt(deploymentID uint) (time.Time, bool) {
	if a.acct == nil {
		return time.Time{}, false
	}
	return a.acct.LastRequestAt(deploymentID)
}

// Sleep orders a deployment to sleep as an operator's deployment-wide order
// does: refused when the deployment cannot sleep, or at L1 when a host
// cannot hold the weights.
func (a *automationAdapter) Sleep(deploymentID uint, level int, held bool) error {
	defer planning.LockLifecycle()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return err
	}
	if code := planning.SleepEligibility(dep); code != "" {
		return messages.NewProductError(code, "the deployment cannot sleep", map[string]any{"engine": dep.Engine})
	}
	if level == proto.SleepLevelL1 {
		insts, err := a.store.InstancesForDeployment(dep.ID)
		if err != nil {
			return err
		}
		if refusal := planning.L1HostMemoryRefusal(a.store, dep, planning.Sleepers(insts), a.hostMemory); refusal != nil {
			return messages.NewProductError(refusal.Code, "the host memory cannot hold the weights an L1 sleep moves there", refusal.Params)
		}
	}
	return a.store.OrderDeploymentSleepHeld(dep.ID, level, proto.SleepModeWait, held)
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
	woken, refused, err := planning.OrderWake(a.store, dep, match, planning.Sleepers(insts), true, true)
	if err != nil {
		return automation.WakeResult{}, err
	}
	result := automation.WakeResult{Woken: uuidList(woken)}
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
	woken, _, err := planning.OrderWake(a.store, dep, match, waiting, true, false)
	if err != nil {
		return nil, err
	}
	return uuidList(woken), nil
}

func (a *automationAdapter) EndWaiting(deploymentID uint, uuids []string) error {
	return a.store.EndWakeWaiting(deploymentID, uuids)
}

// ScaleReplicas sets a single-node deployment's replica count; a multi-node or
// PD deployment's replicas name their nodes, which a schedule does not.
func (a *automationAdapter) ScaleReplicas(deploymentID uint, replicas int) error {
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return err
	}
	if pd, _ := planning.PDOf(dep.Placement); pd != nil || dep.NNodes > 1 {
		return messages.NewProductError(messages.ErrorScheduleReplicasShape, "only a single-node deployment's replica count can be scheduled", nil)
	}
	replicas = min(max(replicas, 1), planning.MaxReplicas)
	if max(dep.Replicas, 1) == replicas {
		return nil
	}
	return a.store.ScaleDeploymentReplicas(dep.ID, dep.Replicas, replicas, nil)
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
