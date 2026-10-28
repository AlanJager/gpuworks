package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
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
