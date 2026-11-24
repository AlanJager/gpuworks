package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// The retry window of a membership grant follows the storage tier's timing:
// unchanged at the tiers' own latency, as long as two election timeouts and
// one commit above it, and never as long as the grant's own timeout.
func TestTheGrantRetryWindowFollowsTheStorageTier(t *testing.T) {
	ms := time.Millisecond
	for name, want := range map[string]time.Duration{
		"standard":           2200 * ms,
		"slow-storage":       2200 * ms,
		"slow-storage@20ms":  2200 * ms,
		"slow-storage@100ms": 3500 * ms,
		"slow-storage@140ms": 4700 * ms,
		// From here the election can outlast the window.
		"slow-storage@141ms": 4700 * ms,
		"slow-storage@200ms": 4700 * ms,
	} {
		tier, err := storagetier.Parse(name)
		if err != nil {
			t.Fatal(err)
		}
		got := GrantRetryWindowFor(tier)
		if got != want {
			t.Errorf("%s: retry window %s, want %s", name, got, want)
		}
		if got >= ClusterMembershipGrantTimeout {
			t.Errorf("%s: retry window %s reaches the grant timeout %s", name, got, ClusterMembershipGrantTimeout)
		}
		if covered := tier.GrantRetryNeed() <= got; covered != (tier.NetworkLatency() <= 140*ms) {
			t.Errorf("%s: the window covers an election: %v", name, covered)
		}
	}
	previous := clusterMembershipRetryWindow
	t.Cleanup(func() { clusterMembershipRetryWindow = previous })
	UseStorageTier("slow-storage@100ms")
	if clusterMembershipRetryWindow != 3500*ms {
		t.Fatalf("a process told its tier keeps a retry window of %s", clusterMembershipRetryWindow)
	}
	UseStorageTier(storagetier.SlowStorage)
	if clusterMembershipRetryWindow != baseGrantRetryWindow {
		t.Fatalf("back at the tier's own timing the window is %s", clusterMembershipRetryWindow)
	}
}
