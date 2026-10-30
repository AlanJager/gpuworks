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

// reservedAgainst is the ledger's reservation for a deployment of yield
// policy being placed: a sleeping instance of a deployment that shares its
// cards by policy (any but never) keeps its whole claim against a policy it
// does not share with (never), so a request that wakes it is never left
// without room. A sleep an operator or a schedule holds reserves nothing: no
// request wakes it, and the wake that does (an operator's, a schedule's)
// waits for room or makes it. Nil when no deployment shares by yield policy.
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
	policies := make(map[uint]string, len(sharing))
	for _, dep := range sharing {
		if dep.WakesOnRequest() {
			policies[dep.ID] = dep.YieldPolicy
		}
	}
	return func(in *store.Instance) bool {
		other, ok := policies[in.DeploymentID]
		return ok && !yieldpolicy.SharesCardsWith(policy, other)
	}
}
