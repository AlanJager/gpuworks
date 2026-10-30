package planning

import (
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment that shares cards by yield policy keeps them while asleep, for
// a request's wake, against a deployment that does not share with it
// (reservedAgainst). When that is the only reason a launch does not fit, the
// refusal says so and names the sleepers, rather than reading as cards in use.

// LaunchReserved is a launch that would fit but for sleepers keeping their
// cards: the cards it would take and the deployments asleep on them.
type LaunchReserved struct {
	GPUIndexes  []int
	Deployments []YieldHolder
}

// launchReserved re-plans req without the reservation; nil when nothing is
// reserved, or the launch does not fit even so.
func launchReserved(st *store.Store, node *store.Node, req scheduler.Request, dtypeOK bool, dtypeReason string, servingUnavailable bool, view ledgerView) *LaunchReserved {
	reserved := view.reserved
	if reserved == nil {
		return nil
	}
	view.reserved = nil
	snap := nodeSnapshot(st, node, dtypeOK, dtypeReason, view)
	snap.ServingUnavailable = servingUnavailable
	result := scheduler.Schedule(req, snap)
	if !result.OK || result.Plan == nil {
		return nil
	}
	insts, err := st.InstancesForNode(node.ID)
	if err != nil {
		return nil
	}
	cards := map[int]bool{}
	for _, index := range result.Plan.GPUIndexes {
		cards[index] = true
	}
	out := &LaunchReserved{GPUIndexes: result.Plan.GPUIndexes}
	seen := map[uint]bool{}
	for i := range insts {
		in := &insts[i]
		if seen[in.DeploymentID] || !reserved(in) || !chargesResidue(st, in) {
			continue
		}
		for _, index := range parseInts(in.GPUIndexes) {
			if !cards[index] {
				continue
			}
			if dep, err := st.GetDeployment(in.DeploymentID); err == nil {
				seen[dep.ID] = true
				out.Deployments = append(out.Deployments, YieldHolder{DeploymentID: dep.ID, Deployment: dep.Name, Policy: dep.Policy(), Instance: in.UUID})
			}
			break
		}
	}
	if len(out.Deployments) == 0 {
		return nil
	}
	return out
}

// ReservedRefusal is the refusal of a launch that only sleepers' cards would
// take: the node, the cards, and each deployment asleep on them with its
// yield policy.
func ReservedRefusal(node *store.Node, reserved *LaunchReserved) scheduler.Reason {
	sleepers := make([]map[string]any, 0, len(reserved.Deployments))
	for _, d := range reserved.Deployments {
		sleepers = append(sleepers, map[string]any{"deployment": d.Deployment, "policy": d.Policy.Yield})
	}
	return scheduler.Reason{Code: scheduler.ReasonReservedForSleepers, Params: map[string]any{
		"node": node.DisplayName(), "gpus": reserved.GPUIndexes, "sleepers": sleepers,
	}}
}
