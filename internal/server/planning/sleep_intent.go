package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
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
