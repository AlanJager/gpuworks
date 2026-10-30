// Package servicelevel is how a deployment shares GPUs with others: its
// service level and the policy each level stands for. It is a leaf, read by
// the store (the column's values), the gateway (how long a request may wait
// for a wake) and the residency arbiter (who may put whom to sleep).
//
// The rule every level follows (docs/explanation/service-levels.md): a
// deployment waiting for its cards may have the one awake on them put to
// sleep when that one can be preempted and either ranks lower, has had its
// turn at the same rank, or has been idle long enough to give the cards up.
package servicelevel

import "time"

// Levels, stored in Deployment.ServiceLevel.
const (
	// Resident never gives its cards up to another deployment. Every
	// deployment from before service levels is resident.
	Resident = "resident"
	// Online gives its cards up only when idle, and takes them back from
	// shared and offline deployments as soon as a request comes.
	Online = "online"
	// Shared takes turns with the other shared deployments on its cards and
	// gives them up to online ones.
	Shared = "shared"
	// Offline gives its cards up to everyone else at once, cutting off the
	// requests it is serving, and resumes when the cards are free again.
	Offline = "offline"
)

// Valid says level is one of the levels.
func Valid(level string) bool {
	switch level {
	case Resident, Online, Shared, Offline:
		return true
	}
	return false
}

// Policy is what a level stands for.
type Policy struct {
	// Rank orders levels: a waiting deployment may preempt one of lower
	// rank.
	Rank int
	// Preemptible says the arbiter may put the deployment to sleep for
	// another while it is busy. A deployment that is not preemptible is
	// put to sleep only when idle (IdleRelease), and a resident one never.
	Preemptible bool
	// TakesTurns says deployments of this level on the same cards take
	// turns: one that has had its MinTurn gives way to an equal that has
	// waited WaitBeforeTurn.
	TakesTurns bool
	// IdleRelease is how long the deployment may sit with no request in
	// flight or waiting before it gives its cards to a deployment waiting
	// for them; zero never.
	IdleRelease time.Duration
	// MinTurn is the least time it keeps its cards once woken before an
	// equal may take them; the arbiter raises it to a multiple of the
	// measured switch time.
	MinTurn time.Duration
	// WaitBeforeTurn is how long an equal's oldest request must have waited
	// before the deployment gives way to it.
	WaitBeforeTurn time.Duration
	// HoldMax is how long the gateway holds a request for the deployment
	// while it wakes.
	HoldMax time.Duration
	// Drain is how long requests in flight may run on once the deployment is
	// to give its cards up; zero cuts them off at once.
	Drain time.Duration
}

// For is the policy of level; an unknown level reads as resident.
func For(level string) Policy {
	switch level {
	case Online:
		return Policy{Rank: 3, IdleRelease: 5 * time.Minute, HoldMax: 2 * time.Minute}
	case Shared:
		return Policy{
			Rank: 2, Preemptible: true, TakesTurns: true, IdleRelease: time.Minute,
			MinTurn: 2 * time.Minute, WaitBeforeTurn: 30 * time.Second, HoldMax: 10 * time.Minute, Drain: 30 * time.Second,
		}
	case Offline:
		return Policy{Rank: 1, Preemptible: true, IdleRelease: 10 * time.Second, HoldMax: 30 * time.Minute}
	}
	return Policy{Rank: 4, HoldMax: 2 * time.Minute}
}

// MinTurnFloor and SwitchMultiple set a shared deployment's turn from the
// measured switch time: max(policy MinTurn, SwitchMultiple × switch), so the
// time spent switching stays a small share of the time spent serving.
const SwitchMultiple = 10

// Turn is the least time a deployment keeps its cards once woken, given the
// switch time measured for it (zero when none is known yet).
func Turn(p Policy, switchTime time.Duration) time.Duration {
	if t := SwitchMultiple * switchTime; t > p.MinTurn {
		return t
	}
	return p.MinTurn
}

// Holder is a deployment awake on cards another one waits for.
type Holder struct {
	Level string
	// AwakeFor is how long it has been awake since its last wake.
	AwakeFor time.Duration
	// IdleFor is how long it has had no request in flight or waiting; zero
	// while it has one.
	IdleFor time.Duration
	// SwitchTime is the measured time to switch it out and back; zero when
	// unknown.
	SwitchTime time.Duration
}

// Waiter is a deployment waiting for cards.
type Waiter struct {
	Level string
	// Waited is how long its oldest request (or its wish to resume) has
	// waited.
	Waited time.Duration
}

// Reason says why a holder may be put to sleep for a waiter.
type Reason string

// Reasons a holder gives way.
const (
	// ReasonIdle: the holder has been idle for its IdleRelease.
	ReasonIdle Reason = "idle"
	// ReasonRank: the waiter ranks higher and the holder is preemptible.
	ReasonRank Reason = "rank"
	// ReasonTurn: equals taking turns, and the holder has had its turn.
	ReasonTurn Reason = "turn"
)

// Yields says whether holder must give its cards to waiter, and why. A
// resident holder never does.
func Yields(holder Holder, waiter Waiter) (Reason, bool) {
	hp, wp := For(holder.Level), For(waiter.Level)
	if holder.Level == Resident || !Valid(holder.Level) {
		return "", false
	}
	if hp.IdleRelease > 0 && holder.IdleFor >= hp.IdleRelease {
		return ReasonIdle, true
	}
	if hp.Preemptible && wp.Rank > hp.Rank {
		return ReasonRank, true
	}
	if hp.TakesTurns && wp.Rank == hp.Rank && holder.AwakeFor >= Turn(hp, holder.SwitchTime) && waiter.Waited >= hp.WaitBeforeTurn {
		return ReasonTurn, true
	}
	return "", false
}

// SharesCardsWith says a deployment of level a may be placed on cards a
// deployment of level b holds (awake or asleep). Two online deployments may
// not share: neither would give way to the other while both are busy, and
// each would hold the other's requests for as long as it stays busy. A
// resident deployment shares with no one but what it already shares by
// sleeping (its own choice).
func SharesCardsWith(a, b string) bool {
	if a == Resident || b == Resident {
		return false
	}
	return !(a == Online && b == Online)
}
