package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Card is one GPU of one node.
type Card struct {
	NodeID uint
	Index  int
}

// InstanceCards are the cards the instances hold a claim on, awake or asleep:
// the same instances the ledger charges (gpuAllocations).
func InstanceCards(insts []store.Instance) map[Card]bool {
	out := map[Card]bool{}
	for i := range insts {
		in := &insts[i]
		if in.State == proto.InstStopped || (in.State == proto.InstFailed && in.Phase != proto.PhaseSleep) {
			continue
		}
		for _, index := range parseInts(in.GPUIndexes) {
			out[Card{NodeID: in.NodeID, Index: index}] = true
		}
	}
	return out
}

// reservedAgainst is the ledger's reservation for a deployment of level being
// placed: a sleeping instance of a deployment that shares its cards by
// service level (any level but resident) keeps its whole claim against a
// level it does not share with. A resident deployment or an online one is
// thus never placed on an online deployment's cards while that one sleeps, so
// its wake is never left without room; a shared or offline one may be. Nil
// when no deployment shares by service level.
func reservedAgainst(st *store.Store, level string) func(*store.Instance) bool {
	if st == nil {
		return nil
	}
	sharing, err := st.SharingDeployments()
	if err != nil || len(sharing) == 0 {
		return nil
	}
	if level == "" {
		level = servicelevel.Resident
	}
	levels := make(map[uint]string, len(sharing))
	for _, dep := range sharing {
		levels[dep.ID] = dep.ServiceLevel
	}
	return func(in *store.Instance) bool {
		other, ok := levels[in.DeploymentID]
		return ok && !servicelevel.SharesCardsWith(level, other)
	}
}
