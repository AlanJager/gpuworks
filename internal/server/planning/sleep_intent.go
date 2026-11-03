package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// InstanceSleepIntent is the sleep level an instance is ordered to: the
// DesiredSleep of its desired state, which a deployment-wide order and an
// order for that instance both write. 0 (awake) when it carries none.
func InstanceSleepIntent(in *store.Instance) int { return in.SleepIntent() }

// InstanceAsleep reports whether an instance may not take requests because of
// sleep: it is ordered to sleep (effective at once, before the engine reports
// the new level, since requests queued behind the order could hang) or it
// still reports a sleep level (a wake that has not finished).
func InstanceAsleep(in *store.Instance) bool {
	return in != nil && (in.SleepLevel > 0 || in.SleepIntent() > 0)
}

// ReportedSleepLevel is the sleep level an instance's engine is at. A
// headless follower reports none of its own: its leader's engine sleeps and
// wakes the whole generation, so a follower is at the level its generation's
// leader reports, and at none while that leader is not running or cannot be
// found (the ledger then keeps the follower's full claim).
func ReportedSleepLevel(st *store.Store, in *store.Instance) int {
	if !in.Headless || in.DistGroup == "" || st == nil {
		return in.SleepLevel
	}
	insts, err := st.InstancesForDeployment(in.DeploymentID)
	if err != nil {
		return 0
	}
	return ReportedSleepLevelAmong(insts, in)
}

// ReportedSleepLevelAmong is ReportedSleepLevel over the deployment's
// instances the caller already holds.
func ReportedSleepLevelAmong(insts []store.Instance, in *store.Instance) int {
	if !in.Headless || in.DistGroup == "" {
		return in.SleepLevel
	}
	for i := range insts {
		leader := &insts[i]
		if !leader.Headless && SameGeneration(*leader, *in) && leader.State == proto.InstRunning {
			return leader.SleepLevel
		}
	}
	return 0
}

// ReplicaWakesOnRequest says in is a replica the gateway refuses for sleep
// (running, its leader if multi-node, asleep) and a request wakes it
// (store.Deployment.WakesOnRequest: dep can sleep and in's sleep is not
// held).
func ReplicaWakesOnRequest(dep *store.Deployment, in *store.Instance) bool {
	return in.State == proto.InstRunning && !in.Headless && InstanceAsleep(in) && dep.WakesOnRequest(in)
}

// RequestWakes says a request for dep that finds no awake replica wakes one
// of insts (ReplicaWakesOnRequest), so it is held for that wake rather than
// refused: what the gateway's route table, the playground and batches say
// of a whole deployment.
func RequestWakes(dep *store.Deployment, insts []store.Instance) bool {
	for i := range insts {
		if ReplicaWakesOnRequest(dep, &insts[i]) {
			return true
		}
	}
	return false
}

// OperatorSleepWakesOnRequest is what an operator's sleep of dep does when its
// order does not say: whether a request wakes the replicas it puts to sleep
// (store.Instance.SleepHeld false) or they stay asleep until someone wakes
// them (held). A deployment that shares its cards by its yield policy (any
// but never) has already agreed to give them up and take them back on
// demand, so a request wakes it as it wakes the platform's own yields; one
// that never yields stays asleep, held. The same rule decides what an
// operator's yielding wake does to the occupants it puts to sleep.
func OperatorSleepWakesOnRequest(dep *store.Deployment) bool {
	return dep != nil && dep.Sleepable && dep.Policy().Yield != yieldpolicy.Never
}
