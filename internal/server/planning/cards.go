package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Card is one GPU of one node.
type Card struct {
	NodeID uint
	Index  int
}

// holdsClaim says the instance holds a claim on its cards, awake or asleep.
// A failed generation outside a sleep is the controller's to remove and holds
// none; a failed sleep report does, since its engine may still own the card
// until the agent confirms it stopped. The ledger, the board, the cards and
// the quota room all charge the same instances.
func holdsClaim(in *store.Instance) bool {
	return in.State != proto.InstStopped && (in.State != proto.InstFailed || in.Phase == proto.PhaseSleep)
}

// InstanceCards are the cards the instances hold a claim on, awake or asleep:
// the same instances the ledger charges (gpuAllocations).
func InstanceCards(insts []store.Instance) map[Card]bool {
	out := map[Card]bool{}
	for i := range insts {
		in := &insts[i]
		if !holdsClaim(in) {
			continue
		}
		for _, index := range parseInts(in.GPUIndexes) {
			out[Card{NodeID: in.NodeID, Index: index}] = true
		}
	}
	return out
}

// SplitByCards splits insts into those that hold a claim on one of cards
// (on) and the rest (off).
func SplitByCards(insts []store.Instance, cards map[Card]bool) (off, on []store.Instance) {
	for i := range insts {
		held := false
		if holdsClaim(&insts[i]) {
			for _, index := range parseInts(insts[i].GPUIndexes) {
				if cards[Card{NodeID: insts[i].NodeID, Index: index}] {
					held = true
					break
				}
			}
		}
		if held {
			on = append(on, insts[i])
		} else {
			off = append(off, insts[i])
		}
	}
	return off, on
}

// HeldGPUs is which GPUs a deployment of yield policy being placed cannot
// take, by node, for ranking nodes and choosing them for units before the
// ledger is asked: those awake instances hold (store.AwakeGPUHolders) and
// those sleepers keep for a request's wake against the policy, by the
// ledger's own rule (reservedAgainst). The sleepers are read only when a
// deployment shares cards by yield policy.
func HeldGPUs(st *store.Store, policy string) (map[uint]map[int]bool, error) {
	held, err := st.AwakeGPUHolders()
	if err != nil {
		return nil, err
	}
	reserved := reservedAgainst(st, policy)
	if reserved == nil {
		return held, nil
	}
	sleepers, err := st.SleepingGPUHolders()
	if err != nil {
		return nil, err
	}
	for i := range sleepers {
		in := &sleepers[i]
		if !reserved(in) {
			continue
		}
		if held[in.NodeID] == nil {
			held[in.NodeID] = map[int]bool{}
		}
		for _, index := range parseInts(in.GPUIndexes) {
			held[in.NodeID][index] = true
		}
	}
	return held, nil
}

// reservedAgainst is the ledger's reservation for a deployment of yield
// policy being placed: a sleeping instance of a deployment that shares its
// cards by policy (any but never) keeps its whole claim against a policy it
// does not share with (never), so a request that wakes it is never left
// without room. A sleeper an operator or a schedule holds reserves nothing
// (store.Deployment.WakesOnRequest, per instance): no request wakes it, and
// the wake that does (an operator's, a schedule's) waits for room or makes
// it. Nil when no deployment shares by yield policy.
func reservedAgainst(st *store.Store, policy string) func(*store.Instance) bool {
	if st == nil {
		return nil
	}
	sharing, err := st.SharingDeployments()
	if err != nil || len(sharing) == 0 {
		return nil
	}
	if policy == "" {
		policy = yieldpolicy.Never
	}
	deps := make(map[uint]*store.Deployment, len(sharing))
	for i := range sharing {
		deps[sharing[i].ID] = &sharing[i]
	}
	return func(in *store.Instance) bool {
		dep := deps[in.DeploymentID]
		return dep != nil && dep.WakesOnRequest(in) && !yieldpolicy.SharesCardsWith(policy, dep.YieldPolicy)
	}
}
