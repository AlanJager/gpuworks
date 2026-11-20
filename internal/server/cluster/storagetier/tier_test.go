package storagetier

import (
	"testing"
	"time"
)

func TestTiersCarryTheirMeasuredTiming(t *testing.T) {
	for _, tc := range []struct {
		tier                       Tier
		latency, shortest, longest time.Duration
		recovery                   time.Duration
		recorded                   string
	}{
		{Standard, 10 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 10 * time.Second, ""},
		{SlowStorage, 50 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond, 6 * time.Second, "slow-storage"},
	} {
		shortest, longest := tc.tier.ElectionTimer()
		if tc.tier.NetworkLatency() != tc.latency || shortest != tc.shortest || longest != tc.longest || tc.tier.ElectionTimeout() != tc.shortest || tc.tier.RecoveryTarget() != tc.recovery || tc.tier.Recorded() != tc.recorded {
			t.Errorf("%s: latency %s timer %s-%s recovery %s recorded %q", tc.tier, tc.tier.NetworkLatency(), shortest, longest, tc.tier.RecoveryTarget(), tc.tier.Recorded())
		}
		if got, ok := ForNetworkLatency(tc.latency); !ok || got != tc.tier {
			t.Errorf("ForNetworkLatency(%s) = %q, %v", tc.latency, got, ok)
		}
	}
	if got := All(); len(got) != 2 || got[0] != Standard || got[1] != SlowStorage {
		t.Fatalf("All() = %v", got)
	}
	if !SlowStorage.Slower(Standard) || Standard.Slower(SlowStorage) || Standard.Slower(Standard) {
		t.Fatal("slow-storage must be the only tier slower than standard")
	}
}

func TestParseAcceptsOnlyTheNamedTiers(t *testing.T) {
	for name, want := range map[string]Tier{"": Standard, "standard": Standard, "slow-storage": SlowStorage} {
		if got, err := Parse(name); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"slow", "Standard", "slow_storage", "100ms", " standard"} {
		if got, err := Parse(name); err == nil {
			t.Errorf("Parse(%q) = %q, want a refusal", name, got)
		}
	}
	for _, latency := range []time.Duration{0, time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
		if tier, ok := ForNetworkLatency(latency); ok {
			t.Errorf("ForNetworkLatency(%s) = %q, want no tier", latency, tier)
		}
	}
	var unset Tier
	if unset.NetworkLatency() != 10*time.Millisecond || unset.Recorded() != "" {
		t.Fatal("an unset tier must behave as standard")
	}
}
