//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"syscall"

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

// materialJoinedBranches finds the first multi-cause error along err's
// single-wrapper chain. An empty join is reported with no branches.
func materialJoinedBranches(err error) ([]error, bool) {
	for cause := err; cause != nil; {
		if joined, multiple := cause.(interface{ Unwrap() []error }); multiple {
			return joined.Unwrap(), true
		}
		wrapped, ok := cause.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		cause = wrapped.Unwrap()
	}
	return nil, false
}

// everyMaterialBranch reports whether a joined cause is non-empty and each of
// its branches satisfies holds.
func everyMaterialBranch(branches []error, holds func(error) bool) bool {
	if len(branches) == 0 {
		return false
	}
	for _, branch := range branches {
		if branch == nil || !holds(branch) {
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
	if branches, joined := materialJoinedBranches(err); joined {
		return !everyMaterialBranch(branches, func(branch error) bool { return !materialReplicationFailureInvalidates(branch) })
	}
	return !materialCheckpointContention(err) && !unavailableMaterialPeer(err)
}

// A peer that cannot be reached contributes no material copy. An
// authenticated response with missing, changed, or misidentified bytes is a
// proof failure, never an unavailable peer.
func unavailableMaterialPeer(err error) bool {
	if branches, joined := materialJoinedBranches(err); joined {
		// One unreachable cause must not excuse another branch's verdict.
		return everyMaterialBranch(branches, unavailableMaterialPeer)
	}
	var partitioned *memberDatabasePartitioned
	var refusal *materialReceiverStatus
	if errors.As(err, &partitioned) || (errors.As(err, &refusal) && refusal.unauthorized) {
		return true
	}
	var requestErr *url.Error
	if !errors.As(err, &requestErr) {
		return false
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	// A connection that closes before or inside the response delivered no
	// answer. A failed certificate or identity check is not in this list.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, context.DeadlineExceeded)
}
