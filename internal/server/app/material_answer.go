//go:build linux && dqlite

package app

import (
	"errors"
	"net/url"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The two questions whose "no answer" includes causes only this package
// knows. Everything else is the store's classifier and its policies.

// proofClusterLost reports a proof that lost its context, its database
// connection, the peer it was reading from, or the majority it needs.
func proofClusterLost(err error) bool {
	return errors.Is(err, errCandidateMaterialNoMajority) || errors.Is(err, errRemoteMaterialBatchUnavailable) ||
		store.ProofClusterLost.Unanswered(err)
}

// peerUnreachable reports a peer that contributed no material copy because
// it could not be reached. An authenticated response with missing, changed
// or misidentified bytes is a proof failure, never an unreachable peer.
func peerUnreachable(err error) bool {
	// One unreachable cause must not excuse another branch's verdict.
	if held, joined := store.EveryBranch(err, peerUnreachable); joined {
		return held
	}
	var partitioned *memberDatabasePartitioned
	var refusal *materialReceiverStatus
	if errors.As(err, &partitioned) || (errors.As(err, &refusal) && refusal.unauthorized) {
		return true
	}
	// Only a request that itself failed: a connection that closes before or
	// inside the response delivered no answer.
	var request *url.Error
	return errors.As(err, &request) && store.PeerUnreachable.Unanswered(err)
}
