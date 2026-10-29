package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// GPUOccupant is one instance on a GPU as the ledger charges it: its full
// claim while awake (or waking, or ordered to sleep and not yet asleep),
// only its residue while it sleeps.
type GPUOccupant struct {
	Instance     store.Instance
	Asleep       bool
	ClaimBytes   int64
	ResidueBytes int64
}

// GPUOccupants are the instances on each GPU of a node, as GPUAllocations
// charges them, by GPU index. ClaimBytes is the instance's own claim on that
// GPU: its memory claim, its dGPU limit and reserve, or the whole card.
func GPUOccupants(st *store.Store, node *store.Node) map[int][]GPUOccupant {
	if st == nil || node == nil {
		return map[int][]GPUOccupant{}
	}
	insts, _ := st.InstancesForNode(node.ID)
	return occupantsOf(st, node, insts, newResidues(st), func(in *store.Instance) bool { return chargesResidue(st, in) })
}

// occupantsOf is GPUOccupants over instances the caller already holds,
// with the same sleeper test as allocateInstances.
func occupantsOf(st *store.Store, node *store.Node, insts []store.Instance, residue *residues, sleeper func(*store.Instance) bool) map[int][]GPUOccupant {
	out := map[int][]GPUOccupant{}
	capacity := map[int]int64{}
	for _, gpu := range node.OwnedGPUs() {
		capacity[gpu.Index] = gpu.MemoryTotal
	}
	_, _, archFamily, _ := nodeGPUProfile(node)
	for i := range insts {
		in := &insts[i]
		if in.State == proto.InstStopped || (in.State == proto.InstFailed && in.Phase != proto.PhaseSleep) {
			continue
		}
		claim := instanceClaim(st, in, archFamily)
		asleep := sleeper(in)
		for _, index := range parseInts(in.GPUIndexes) {
			occupant := GPUOccupant{Instance: *in, Asleep: asleep, ClaimBytes: claim}
			if claim <= 0 {
				occupant.ClaimBytes = capacity[index]
			}
			if asleep {
				occupant.ResidueBytes = residue.of(in)
			}
			out[index] = append(out[index], occupant)
		}
	}
	return out
}
