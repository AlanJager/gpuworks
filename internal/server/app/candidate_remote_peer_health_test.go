//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func healthTestPeers() []store.ClusterTransportMember {
	return []store.ClusterTransportMember{
		{ID: 1, EnrollmentOrigin: "https://peer-a"},
		{ID: 2, EnrollmentOrigin: "https://peer-b"},
		{ID: 3, EnrollmentOrigin: "https://peer-c"},
	}
}

func healthTestOrder(peers []store.ClusterTransportMember) []uint64 {
	ids := make([]uint64, 0, len(peers))
	for _, peer := range peers {
		ids = append(ids, peer.ID)
	}
	return ids
}

func TestMaterialPeerHealthOrdersRecentlyFailedPeerLast(t *testing.T) {
	now := time.Now().UTC()
	for _, peer := range healthTestPeers() {
		forgetMaterialPeerFailure(peer.EnrollmentOrigin)
	}
	peers := healthTestPeers()
	if got := healthTestOrder(orderMaterialProofPeers(peers, now)); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("healthy order changed: %v", got)
	}
	rememberMaterialPeerFailure("https://peer-a", now)
	if got := healthTestOrder(orderMaterialProofPeers(peers, now)); !slices.Equal(got, []uint64{2, 3, 1}) {
		t.Fatalf("failed low-ID peer stayed primary: %v", got)
	}
	rememberMaterialPeerFailure("https://peer-c", now)
	if got := healthTestOrder(orderMaterialProofPeers(peers, now)); !slices.Equal(got, []uint64{2, 1, 3}) {
		t.Fatalf("two failed peers lost their relative order: %v", got)
	}
	if got := healthTestOrder(orderMaterialProofPeers(peers, now.Add(remoteMaterialPeerFailureRetention+time.Second))); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("expired failures kept peers deprioritized: %v", got)
	}
	rememberMaterialPeerFailure("https://peer-b", now)
	forgetMaterialPeerFailure("https://peer-b")
	if got := healthTestOrder(orderMaterialProofPeers(peers, now)); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("verified peer stayed deprioritized: %v", got)
	}
	if got := healthTestOrder(peers); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("ordering mutated the membership slice: %v", got)
	}
}

func TestRecordMaterialPeerOutcomeIgnoresQuorumRaceCancellation(t *testing.T) {
	const endpoint = "https://peer-outcome"
	forgetMaterialPeerFailure(endpoint)
	t.Cleanup(func() { forgetMaterialPeerFailure(endpoint) })
	now := time.Now().UTC()

	recordMaterialPeerOutcome(endpoint, context.Background(), errors.New("connection refused"))
	if !materialPeerFailedRecently(endpoint, now) {
		t.Fatal("genuine peer failure was not remembered")
	}
	recordMaterialPeerOutcome(endpoint, context.Background(), nil)
	if materialPeerFailedRecently(endpoint, now) {
		t.Fatal("verified peer kept its failure record")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	recordMaterialPeerOutcome(endpoint, canceled, canceled.Err())
	if materialPeerFailedRecently(endpoint, now) {
		t.Fatal("quorum-race cancellation counted as peer failure")
	}
	recordMaterialPeerOutcome(endpoint, context.Background(), context.Canceled)
	if materialPeerFailedRecently(endpoint, now) {
		t.Fatal("canceled read counted as peer failure")
	}
}
