//go:build linux && dqlite

package app

import (
	"gpuworks.ai/gpuworks/internal/server/store"
)

// observedMembersOnlyShrink reports a loss of previously observed members.
// A shorter set whose IDs all came from the previous observation is not a new
// publisher. Callers still run the next proof; a definitive failure can close.
func observedMembersOnlyShrink(previous, current []store.ClusterTransportMember) bool {
	if len(previous) == 0 || len(current) == 0 || len(current) >= len(previous) {
		return false
	}
	seen := make(map[uint64]struct{}, len(previous))
	for _, member := range previous {
		seen[member.ID] = struct{}{}
	}
	for _, member := range current {
		if _, ok := seen[member.ID]; !ok {
			return false
		}
	}
	return true
}

// Background contention is not evidence that material changed. The failed
// repair remains pending; this classification grants no authority and does not
// change the finite lifetime of the last complete proof.
func materialReplicationFailureInvalidates(err error) bool {
	if err == nil {
		return false
	}
	// A second cause may describe changed bytes or an uncertain commit. Do not
	// let an unavailable peer or checkpoint hide that failure: a joined cause
	// closes unless every branch on its own would not.
	if harmless, joined := store.EveryBranch(err, func(branch error) bool { return !materialReplicationFailureInvalidates(branch) }); joined {
		return !harmless
	}
	return !materialCheckpointContention(err) && !peerUnreachable(err) && !store.ReplicationUndecided.Unanswered(err)
}
