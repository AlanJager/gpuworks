// ledger_foreign.go is the ledger's view of cards that processes outside the
// platform hold (ADR 0068): what they take from a card and whether a partition
// may share it with them.

package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// foreignShare is what processes outside the platform hold on one of the
// node's cards and whether a partition may share it with them by memory
// (store.GPU.SharesMemoryWithForeign), which also asks that the node's agent
// keeps the holders reported once an instance of ours joins them
// (proto.PartitionSafetyForeignTracked): an older one drops them, and the
// card would read as free after the first placement. The holders' list is
// parsed only for a held card, once per snapshot.
func foreignShare(node *store.Node, g *store.GPU) (int64, bool) {
	if !g.HeldOutsidePlatform() {
		return 0, false
	}
	bytes, shareable := g.SharesMemoryWithForeign()
	if !shareable && g.CanTakeWork() {
		// An operator allowed work on the card (or it is faulty and allowed):
		// it is not shared by this rule, but what the holders take is still
		// not on offer to a partition.
		bytes, _ = g.ForeignBytes()
	}
	return bytes, shareable && node.PartitionSafetyProtocol >= proto.PartitionSafetyForeignTracked
}

// ForeignHold is the memory on one of the node's cards that processes
// outside the platform hold (held) and the headroom the deployment policy
// keeps beside it: together, what neither admission
// (scheduler.GPU.Available), a quota change in place (QuotaRoom) nor the GPU
// list's free figure may offer to the platform. Both zero when no holder's
// memory is counted. known is false on a held card that takes no work and
// whose holders' memory is not known (store.GPU.ForeignBytes): the card
// offers nothing beyond what is placed on it already, as admission refuses
// it outright, rather than reading the unknown as nothing held.
func ForeignHold(node *store.Node, g *store.GPU, policy store.DeploymentPolicy) (held, headroom int64, known bool) {
	if g.HeldOutsidePlatform() && !g.CanTakeWork() {
		if _, ok := g.ForeignBytes(); !ok {
			return 0, 0, false
		}
	}
	held, _ = foreignShare(node, g)
	if held <= 0 {
		return 0, 0, true
	}
	return held, foreignHeadroom(policy, g.MemoryTotal), true
}

// foreignHeadroom is the policy's headroom on a card of that size.
func foreignHeadroom(policy store.DeploymentPolicy, totalBytes int64) int64 {
	return estimate.ForeignHeadroomBytes(totalBytes, policy.ForeignHeadroomPct, policy.ForeignHeadroomMinBytes)
}
