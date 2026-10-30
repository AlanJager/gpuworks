package planning

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica of a deployment that shares cards by yield policy may start on
// cards other such deployments hold awake: it waits for them as a request
// waits for a sleeping deployment, and the residency arbiter puts them to
// sleep by the same rule (docs/explanation/service-levels.md). launchYield
// is the planning side: whether the launch fits with them asleep, and which.

// LaunchYield is how a launch that does not fit would: the awake instances
// on the cards it would take, of deployments that share them with it, put to
// sleep. No holders is a launch that waits only for sleeps already ordered.
type LaunchYield struct {
	Holders []YieldHolder
	// GPUIndexes are the cards it would take.
	GPUIndexes []int
}

// YieldHolder is an awake instance a launch waits for, and its deployment.
type YieldHolder struct {
	DeploymentID uint
	Deployment   string
	Policy       servicelevel.Policy
	Instance     string
}

// launchYield re-plans req with the awake instances of the deployments that
// share cards with a deployment of policy asleep, as the ledger will charge
// them once they have slept; nil when policy shares with none, nothing would
// sleep, or the launch would not fit even so. Sharing deployments are
// single-node, so an instance is a whole replica.
func launchYield(st *store.Store, node *store.Node, policy string, req scheduler.Request, dtypeOK bool, dtypeReason string, servingUnavailable bool, view ledgerView) *LaunchYield {
	if policy == "" || policy == servicelevel.Never {
		return nil
	}
	sharing, err := st.SharingDeployments()
	if err != nil || len(sharing) == 0 {
		return nil
	}
	shares := make(map[uint]*store.Deployment, len(sharing))
	for i := range sharing {
		if servicelevel.SharesCardsWith(policy, sharing[i].YieldPolicy) {
			shares[sharing[i].ID] = &sharing[i]
		}
	}
	if len(shares) == 0 {
		return nil
	}
	insts, err := st.InstancesForNode(node.ID)
	if err != nil {
		return nil
	}
	// The awake ones are what it waits for; one already ordered to sleep is
	// counted asleep too, as it will be in a moment, but waits for nothing.
	var awake []*store.Instance
	asleep := map[string]bool{}
	for i := range insts {
		in := &insts[i]
		if shares[in.DeploymentID] == nil || view.excludes(in) || in.State != proto.InstRunning || in.SleepLevel > 0 {
			continue
		}
		if InstanceSleepIntent(in) == 0 {
			awake = append(awake, in)
		}
		asleep[in.UUID] = true
	}
	if len(asleep) == 0 {
		return nil
	}
	view.alsoAsleep = func(in *store.Instance) bool { return asleep[in.UUID] }
	snap := nodeSnapshot(st, node, dtypeOK, dtypeReason, view)
	snap.ServingUnavailable = servingUnavailable
	result := scheduler.Schedule(req, snap)
	if !result.OK || result.Plan == nil {
		return nil
	}
	cards := map[int]bool{}
	for _, index := range result.Plan.GPUIndexes {
		cards[index] = true
	}
	out := &LaunchYield{GPUIndexes: result.Plan.GPUIndexes}
	for _, in := range awake {
		for _, index := range parseInts(in.GPUIndexes) {
			if cards[index] {
				dep := shares[in.DeploymentID]
				out.Holders = append(out.Holders, YieldHolder{DeploymentID: dep.ID, Deployment: dep.Name, Policy: dep.Policy(), Instance: in.UUID})
				break
			}
		}
	}
	return out
}

// YieldRefusal is the refusal of a launch that waits for holders to give up
// node's cards: each holder deployment once, with the terms it gives way to
// a deployment of policy on (servicelevel.TermsOf), and the instances.
func YieldRefusal(node *store.Node, policy servicelevel.Policy, yield *LaunchYield) scheduler.Reason {
	var holders []map[string]any
	var instances []string
	seen := map[uint]bool{}
	for _, h := range yield.Holders {
		instances = append(instances, h.Instance)
		if seen[h.DeploymentID] {
			continue
		}
		seen[h.DeploymentID] = true
		terms := servicelevel.TermsOf(h.Policy, policy)
		entry := map[string]any{"deployment": h.Deployment, "at_once": terms.AtOnce}
		if !terms.AtOnce {
			entry["idle_sec"] = int(terms.Idle / time.Second)
			if terms.Turn > 0 {
				entry["max_serve_sec"] = int(terms.Turn / time.Second)
			}
		}
		holders = append(holders, entry)
	}
	return scheduler.Reason{Code: scheduler.ReasonWaitingForYield, Params: map[string]any{
		"node": node.DisplayName(), "holders": holders, "instances": instances,
	}}
}
