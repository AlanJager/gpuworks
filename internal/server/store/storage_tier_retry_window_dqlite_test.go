//go:build linux && dqlite

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"

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
	for _, tier := range storagetier.All() {
		if need := tier.GrantRetryNeed(); need > clusterMembershipRetryWindow() || need > GrantRetryWindowFor(tier) {
			t.Errorf("storage tier %s: longest election timer plus one commit is %s, beyond the retry window %s (%s for the tier); the retry window must grow with the tier", tier, need, clusterMembershipRetryWindow(), GrantRetryWindowFor(tier))
		}
	}
	if window := clusterMembershipRetryWindow(); window >= ClusterMembershipGrantTimeout {
		t.Fatalf("retry window %s must stay inside the grant timeout %s", window, ClusterMembershipGrantTimeout)
	}
}

// A process that has already started a database node and is then told
// another tier follows what it was told. The node here is a real one,
// started by this test, so the order holds whatever ran before it in the
// package: with the node's tier still in force the window stayed at 2.2 s.
func TestATierToldAfterANodeStartedSetsTheRetryWindow(t *testing.T) {
	previous := cluster.ProcessStorageTier()
	t.Cleanup(func() { cluster.UseStorageTier(previous) })
	cluster.UseStorageTier(storagetier.SlowStorage)
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := OpenRuntimeWith(ctx, filepath.Join(dir, "gpuworks.db"), bus.New(), OpenOptions{Backend: ReplicatedBackend()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := cluster.ProcessStorageTier(); got != storagetier.SlowStorage || clusterMembershipRetryWindow() != baseGrantRetryWindow {
		t.Fatalf("with a node started at slow-storage the process reports %s and a window of %s", got, clusterMembershipRetryWindow())
	}
	cluster.UseStorageTier("slow-storage@100ms")
	if got := clusterMembershipRetryWindow(); cluster.ProcessStorageTier() != "slow-storage@100ms" || got != 3500*time.Millisecond {
		t.Fatalf("told slow-storage@100ms after the node started, the process reports %s and a window of %s; want 3.5s", cluster.ProcessStorageTier(), got)
	}
}
