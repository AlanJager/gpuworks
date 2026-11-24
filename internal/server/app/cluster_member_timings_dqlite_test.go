//go:build linux && dqlite

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// The other members' timing is read from each member itself. A member that
// cannot be asked carries the reason and no tier, and without the cluster's
// membership nothing is returned: the caller must refuse, not guess.
func TestClusterMemberTimingsNeverGuessAMembersTier(t *testing.T) {
	f := newStatusProofFixture(t)
	previous := statusQuorumFetch
	t.Cleanup(func() { statusQuorumFetch = previous })

	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{}, false
	}
	if timings, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, time.Second); err == nil || timings != nil ||
		!strings.Contains(err.Error(), "membership could not be read") {
		t.Fatalf("without the membership: %v, %v", timings, err)
	}

	statusQuorumFetch = func(context.Context, clusterStatus, time.Duration) (enrollment.ClusterQuorumFacts, bool) {
		return enrollment.ClusterQuorumFacts{LeaderID: 3, Members: []enrollment.ClusterMember{
			{ID: 1, Address: f.self.address, Role: "voter", EnrollmentOrigin: "https://127.0.0.1:1"},
			{ID: 2, Address: f.follower.address, Role: "voter"},
			// Nothing listens on port 1.
			{ID: 3, Address: f.leader.address, Role: "voter", EnrollmentOrigin: "https://127.0.0.1:1"},
		}}, true
	}
	timings, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, time.Second)
	if err != nil || len(timings) != 2 {
		t.Fatalf("timings %+v, %v; want the two other members", timings, err)
	}
	if timings[0].ID != 2 || timings[0].Address != f.follower.address || timings[0].Tier != "" || !strings.Contains(timings[0].Unread, "names no enrollment listener") {
		t.Fatalf("a member without a listener: %+v", timings[0])
	}
	if timings[1].ID != 3 || timings[1].Tier != "" || !strings.Contains(timings[1].Unread, "it did not answer") {
		t.Fatalf("a member that does not answer: %+v", timings[1])
	}
	if _, err := ClusterMemberTimings(context.Background(), Config{DataDir: f.root}, 0); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
	if _, err := ClusterMemberTimings(context.Background(), Config{DataDir: t.TempDir()}, time.Second); err == nil {
		t.Fatal("an installation without a member identity was asked for its peers")
	}
}
