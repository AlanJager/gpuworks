//go:build linux && dqlite

package app

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The two questions whose "no answer" includes causes only this package
// knows: the store's policy, and the cause added to it.

// proofClusterLostPolicy: the proof lost its context, its database
// connection, the peer it was reading from, or the majority it needs.
var proofClusterLostPolicy = store.ProofClusterLost.Also(func(err error) bool {
	return errors.Is(err, errCandidateMaterialNoMajority) || errors.Is(err, errRemoteMaterialBatchUnavailable)
})

// peerUnreachablePolicy: a peer contributed no material copy because it
// could not be reached, by the request's own failure or by what its answer
// said: a receiver whose database port timed out, or one that refused
// before it inspected anything. An authenticated response with missing,
// changed or misidentified bytes is a proof failure, never an unreachable
// peer.
var peerUnreachablePolicy = store.PeerUnreachable.Also(func(err error) bool {
	var partitioned *memberDatabasePartitioned
	var refusal *materialReceiverStatus
	return errors.As(err, &partitioned) || (errors.As(err, &refusal) && refusal.unauthorized)
})

func proofClusterLost(err error) bool { return proofClusterLostPolicy.Unanswered(err) }

func peerUnreachable(err error) bool { return peerUnreachablePolicy.Unanswered(err) }
