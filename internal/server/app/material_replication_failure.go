//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
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
	return !materialCheckpointContention(err) && !unavailableMaterialPeer(err) && !materialOutcomeUndecided(err)
}

// materialOutcomeUndecided reports a failure that carries no verdict on this
// member's material: the pass ran out of its own time, the database had no
// leader to ask, the leader could not be read in time, a connection was lost
// under a commit, or too few of the other members answered for a majority.
// The pass stays pending; the proof remains the authority for admission and
// closes the member on a difference. A second cause joined to one of these
// keeps its own verdict. The caller's cancellation and an error nothing
// classifies are not undecided.
func materialOutcomeUndecided(err error) bool {
	if _, joined := err.(interface{ Unwrap() []error }); !joined &&
		(store.ReplicatedLeaderUnavailable(err) || errors.Is(err, store.ErrClusterMembershipObservationTimeout) ||
			errors.Is(err, driver.ErrBadConn) || errors.Is(err, store.ErrMaterialManifestNoMajority)) {
		return true
	}
	for err != nil {
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err == context.DeadlineExceeded
		}
		err = wrapped.Unwrap()
	}
	return false
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
