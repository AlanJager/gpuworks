package app

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// yieldLevel is the level the arbiter puts a deployment to sleep at when it
// gives its cards to another. L2: measured on an L20, an L2 switch with the
// model files in page cache takes 3-5 s against L1's 2-3 s, while an engine
// that has slept at L1 keeps about twice its weights pinned in host memory
// even awake, evicting the other models' files from page cache and turning
// their wakes into disk reads (docs/explanation/service-levels.md).
const yieldLevel = proto.SleepLevelL2

// residencyAdapter backs the residency arbiter with the store, the ledger and
// the gateway's held requests and traffic.
type residencyAdapter struct {
	store *store.Store
	proxy *proxy.Proxy
	acct  *proxy.Accountant
}

// Waiters are the deployments with requests held for their wake whose sleep a
// request may end.
func (a *residencyAdapter) Waiters() ([]automation.Waiter, error) {
	var out []automation.Waiter
	for _, id := range a.proxy.HeldDeployments() {
		n, since := a.proxy.HeldRequests(id)
		if n == 0 {
			continue
		}
		dep, err := a.store.GetDeployment(id)
		if err != nil {
			continue
		}
		if !wakeableByRequest(dep) {
			continue
		}
		out = append(out, automation.Waiter{DeploymentID: id, Level: dep.ServiceLevel, Since: since})
	}
	return out, nil
}

func wakeableByRequest(dep *store.Deployment) bool {
	return dep.State == store.DeployActive && dep.Sleepable && !dep.SleepHeld
}

// Wakeable says the deployment is active, asleep by the platform's choice,
// and has a sleeper to wake.
func (a *residencyAdapter) Wakeable(id uint) (string, bool) {
	dep, err := a.store.GetDeployment(id)
	if err != nil || !wakeableByRequest(dep) {
		return "", false
	}
	insts, err := a.store.InstancesForDeployment(id)
	if err != nil {
		return "", false
	}
	for i := range insts {
		if store.HoldsSleepIntent(&insts[i]) && planning.InstanceSleepIntent(&insts[i]) > 0 {
			return dep.ServiceLevel, true
		}
	}
	return "", false
}

// Blocking is what keeps the first of the deployment's waiting replicas from
// waking: the awake replicas on the cards it needs, one per deployment, or an
// occupant nothing can put to sleep. One replica is enough to serve the
// requests that wait; the rest wake when there is room.
func (a *residencyAdapter) Blocking(id uint) (automation.Blocking, error) {
	dep, err := a.store.GetDeployment(id)
	if err != nil {
		return automation.Blocking{}, err
	}
	insts, err := a.store.InstancesForDeployment(id)
	if err != nil {
		return automation.Blocking{}, err
	}
	var waiting []store.Instance
	for _, in := range insts {
		if in.WakeWaitingSince != nil && store.HoldsSleepIntent(&in) && planning.InstanceSleepIntent(&in) > 0 {
			waiting = append(waiting, in)
		}
	}
	if len(waiting) == 0 {
		return automation.Blocking{}, nil
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return automation.Blocking{}, err
	}
	_, refused, err := planning.AdmitWake(a.store, planning.WakeGroups(match, waiting), true)
	if err != nil || len(refused) == 0 {
		return automation.Blocking{}, err
	}
	targets, blockers, err := planning.YieldTargets(a.store, refused[:1])
	if err != nil {
		return automation.Blocking{}, err
	}
	out := automation.Blocking{Blocked: len(blockers) > 0}
	at := map[uint]int{}
	for _, target := range targets {
		members := []store.Instance{target.Instance}
		targetInsts, err := a.store.InstancesForDeployment(target.Deployment.ID)
		if err != nil {
			return automation.Blocking{}, err
		}
		if targetMatch, err := planning.DeploymentMatch(a.store, target.Deployment, targetInsts); err == nil {
			if slot := targetMatch.SlotOf(target.Instance.UUID); slot != nil {
				members = planning.ReplicaMembers(slot)
			}
		}
		index, ok := at[target.Deployment.ID]
		if !ok {
			at[target.Deployment.ID] = len(out.Holders)
			out.Holders = append(out.Holders, automation.Holder{
				DeploymentID: target.Deployment.ID, Name: target.Deployment.Name, Level: target.Deployment.ServiceLevel,
			})
			index = len(out.Holders) - 1
		}
		for _, member := range members {
			out.Holders[index].Instances = append(out.Holders[index].Instances, member.UUID)
		}
	}
	return out, nil
}

// Activity is the deployment's traffic through the gateway.
func (a *residencyAdapter) Activity(id uint) automation.Activity {
	held, _ := a.proxy.HeldRequests(id)
	last, _ := a.acct.LastRequestAt(id)
	return automation.Activity{InFlight: a.acct.InFlight(id), Held: held, LastRequest: last}
}

// Yield puts the holder's replica to sleep at yieldLevel, a sleep the
// platform chose (not held: a request for it waits and wakes it again).
func (a *residencyAdapter) Yield(id uint, instances []string, abort bool) (int, error) {
	defer planning.LockLifecycle()()
	mode := proto.SleepModeWait
	if abort {
		mode = proto.SleepModeAbort
	}
	if err := a.store.OrderInstancesSleepHeld(id, instances, yieldLevel, mode, false); err != nil {
		return 0, err
	}
	return yieldLevel, nil
}

func (a *residencyAdapter) Drain(id uint, on bool) {
	a.proxy.SetDraining(id, on)
}
