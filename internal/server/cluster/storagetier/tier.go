// Package storagetier names the two timing profiles of a control-plane
// cluster. A tier is chosen when the cluster is installed, from how slowly its
// members' storage acknowledges a synchronous write, and every member must run
// the same one: it sets how long the database waits before it elects a new
// leader.
package storagetier

import (
	"fmt"
	"time"
)

// Tier is a cluster's timing profile.
type Tier string

const (
	// Standard assumes storage whose synchronous writes do not stall for as
	// long as its leader step-down time.
	Standard Tier = "standard"
	// SlowStorage tolerates stalls of several hundred milliseconds: fewer
	// leader changes while idle, a slower change when a leader really fails.
	SlowStorage Tier = "slow-storage"
)

// electionFactor is how the database derives its election timeout from the
// network latency it is given. A follower waits between one and two timeouts
// before it campaigns; a leader steps down after one without a majority.
const electionFactor = 15

type profile struct {
	latency  time.Duration
	recovery time.Duration
}

// The latencies are the only values offered. A larger one makes the election
// outlast the membership grant's retry window, which then has to grow with
// it; the store's tests hold every tier to that relation.
//
// The recovery targets are what was measured when a leader was killed on
// four-core members with networked storage (five runs each): 0.8 to 2.1 s at
// 10 ms and 1.7 to 3.1 s at 50 ms.
var profiles = map[Tier]profile{
	Standard:    {latency: 10 * time.Millisecond, recovery: 10 * time.Second},
	SlowStorage: {latency: 50 * time.Millisecond, recovery: 6 * time.Second},
}

// All lists the tiers from the fastest timing to the slowest.
func All() []Tier { return []Tier{Standard, SlowStorage} }

// Parse reads a recorded or configured tier. The empty string is Standard: a
// cluster installed before tiers existed records none.
func Parse(name string) (Tier, error) {
	if name == "" {
		return Standard, nil
	}
	if _, ok := profiles[Tier(name)]; !ok {
		return "", fmt.Errorf("unknown storage tier %q: want %s or %s", name, Standard, SlowStorage)
	}
	return Tier(name), nil
}

// ForNetworkLatency is the tier that runs the database at this latency.
func ForNetworkLatency(latency time.Duration) (Tier, bool) {
	for _, tier := range All() {
		if profiles[tier].latency == latency {
			return tier, true
		}
	}
	return "", false
}

// Recorded is what a member stores for the tier: nothing for Standard, so a
// standard cluster's files are those of a cluster installed before tiers.
func (t Tier) Recorded() string {
	if t == Standard || t == "" {
		return ""
	}
	return string(t)
}

// NetworkLatency is the latency the database node is started with.
func (t Tier) NetworkLatency() time.Duration { return profiles[t.known()].latency }

// ElectionTimeout is how long a leader goes without a majority before it
// steps down, and the shortest a follower waits before it campaigns.
func (t Tier) ElectionTimeout() time.Duration { return t.NetworkLatency() * electionFactor }

// ElectionTimer is the range a follower's randomized election timer is drawn
// from.
func (t Tier) ElectionTimer() (shortest, longest time.Duration) {
	return t.ElectionTimeout(), 2 * t.ElectionTimeout()
}

// RecoveryTarget is the time within which a write succeeds again after the
// leader's process is killed.
func (t Tier) RecoveryTarget() time.Duration { return profiles[t.known()].recovery }

// Slower reports whether t waits longer than other before electing a leader.
func (t Tier) Slower(other Tier) bool { return t.NetworkLatency() > other.NetworkLatency() }

func (t Tier) known() Tier {
	if _, ok := profiles[t]; ok {
		return t
	}
	return Standard
}
