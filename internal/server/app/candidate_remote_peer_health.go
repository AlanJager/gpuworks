//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// remoteMaterialPeerFailureRetention bounds how long one failed read keeps an
// endpoint deprioritized. The next successful proof read clears the entry
// earlier; expiry alone never authorizes anything, it only restores the
// original membership order for a fresh attempt.
const remoteMaterialPeerFailureRetention = 30 * time.Second

// remoteMaterialPeerFailures remembers each material endpoint's most recent
// genuine proof read failure so the next proof's hedged quorum spends its
// primary slot on a likely-live peer instead of a half-open one.
var remoteMaterialPeerFailures sync.Map

func rememberMaterialPeerFailure(endpoint string, at time.Time) {
	if endpoint == "" {
		return
	}
	remoteMaterialPeerFailures.Store(endpoint, at)
}

func forgetMaterialPeerFailure(endpoint string) {
	remoteMaterialPeerFailures.Delete(endpoint)
}

func materialPeerFailedRecently(endpoint string, now time.Time) bool {
	value, ok := remoteMaterialPeerFailures.Load(endpoint)
	if !ok {
		return false
	}
	at, isTime := value.(time.Time)
	if !isTime || now.Sub(at) >= remoteMaterialPeerFailureRetention {
		remoteMaterialPeerFailures.Delete(endpoint)
		return false
	}
	return true
}

// orderMaterialProofPeers keeps the membership order among equally healthy
// peers and moves endpoints whose recent proof read failed behind the rest.
// The quorum requirement is unchanged; only the hedged attempt order moves.
func orderMaterialProofPeers(peers []store.ClusterTransportMember, now time.Time) []store.ClusterTransportMember {
	ordered := slices.Clone(peers)
	slices.SortStableFunc(ordered, func(a, b store.ClusterTransportMember) int {
		aFailed, bFailed := materialPeerFailedRecently(a.EnrollmentOrigin, now), materialPeerFailedRecently(b.EnrollmentOrigin, now)
		switch {
		case aFailed == bFailed:
			return 0
		case aFailed:
			return 1
		default:
			return -1
		}
	})
	return ordered
}

// recordMaterialPeerOutcome clears a verified peer and remembers a genuine
// failure. A canceled loser of the quorum race proves nothing about the peer.
func recordMaterialPeerOutcome(endpoint string, workCtx context.Context, err error) {
	switch {
	case err == nil:
		forgetMaterialPeerFailure(endpoint)
	case workCtx.Err() != nil || errors.Is(err, context.Canceled):
	default:
		rememberMaterialPeerFailure(endpoint, time.Now().UTC())
	}
}
