package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// YieldPolicyRefusal says why dep may hold no yield policy but never, ""
// when it may (docs/explanation/service-levels.md). A policy that gives way
// puts the deployment to sleep, so it must be able to sleep - the codes of
// SleepEligibility - and only a single-node deployment that is not PD gives
// its cards to another (messages.ErrorYieldPolicyShape). The API refuses a
// policy by it and the deployment DTO tells a client whether to offer one.
func YieldPolicyRefusal(dep *store.Deployment) string {
	pd, _ := PDOf(dep.Placement)
	return DraftYieldPolicyRefusal(dep.Sleepable, dep.Engine, dep.NNodes, pd != nil)
}

// DraftYieldPolicyRefusal is YieldPolicyRefusal for a deployment described
// by its request rather than stored: whether it asks to sleep, its engine,
// how many nodes a replica spans and whether it is PD.
func DraftYieldPolicyRefusal(sleepable bool, engine string, nnodes int, pd bool) string {
	if code := sleep.Eligible(sleep.Deployment{Sleepable: sleepable, Caps: deploy.SleepCapsForEngine(engine)}); code != "" {
		return code
	}
	if nnodes > 1 || pd {
		return messages.ErrorYieldPolicyShape
	}
	return ""
}
