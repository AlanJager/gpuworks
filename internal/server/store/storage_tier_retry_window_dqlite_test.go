//go:build linux && dqlite

package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// A membership grant interrupted by a leadership change is repeated inside
// clusterMembershipRetryWindow. A follower campaigns up to two election
// timeouts after the leader is gone, and the repeated grant then needs one
// replicated commit. A storage tier whose election outlasts the window turns
// every leader change into refused requests, so a slower tier may only be
// added together with a longer window, and the window is capped by the time
// an isolated member must refuse within.
func TestEveryStorageTierElectsInsideTheMembershipRetryWindow(t *testing.T) {
	const oneCommit = 500 * time.Millisecond
	for _, tier := range storagetier.All() {
		_, longest := tier.ElectionTimer()
		if need := longest + oneCommit; need > clusterMembershipRetryWindow {
			t.Errorf("storage tier %s: longest election timer %s plus one commit (%s) is %s, beyond clusterMembershipRetryWindow %s; the retry window must grow with the tier", tier, longest, oneCommit, need, clusterMembershipRetryWindow)
		}
	}
	if clusterMembershipRetryWindow >= ClusterMembershipGrantTimeout {
		t.Fatalf("retry window %s must stay inside the grant timeout %s", clusterMembershipRetryWindow, ClusterMembershipGrantTimeout)
	}
}
