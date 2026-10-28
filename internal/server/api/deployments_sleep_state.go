package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The states a deployment shows while sleep covers every replica that could
// serve it. They are derived, never stored: an instance's reported SleepLevel
// and the sleep intent of its desired state are the only record of sleep, and
// planning.InstanceAsleep is how the gateway judges a request, so the state a
// list shows cannot disagree with what the gateway answers.
const (
	// deploymentStateSleeping is every serving replica asleep while at least
	// one engine still reports the level it had before the order: the order is
	// accepted and the gateway already refuses every request, and the engines
	// are still swapping memory out (a level-1 order takes seconds).
	deploymentStateSleeping = "sleeping"
	// deploymentStateAsleep is every serving replica asleep with the level its
	// engine reports.
	deploymentStateAsleep = "asleep"
)

// deploymentSleep is what a deployment's serving replicas say about sleep.
// Every fact is read from the instances the response already loaded, so it
// costs no query and cannot race a separate read of them.
type deploymentSleep struct {
	// Replicas counts the serving replicas these facts are about: an ordinary
	// replica whose leader and every follower rank run on online nodes, or a PD
	// unit of a role. A deployment with none (no instance yet, or none able to
	// serve) is not asleep, whatever its instances were once ordered to do.
	Replicas int
	// AsleepReplicas counts the serving replicas the gateway refuses because of
	// sleep. It equals Replicas when the whole deployment sleeps; short of it,
	// it is what tells a partly slept deployment from a broken replica, so the
	// running ones stay visible.
	AsleepReplicas int
	// Settled counts the asleep replicas whose engine reports the level it was
	// ordered to. The rest are still swapping memory out: ordered, refused, and
	// not yet asleep.
	Settled int
	// Level is the deepest sleep level an engine reports.
	Level int
}

// Asleep reports every serving replica being asleep, the condition the
// sleeping and asleep states describe. A deployment with no serving replica is
// never asleep: nothing is left to wake.
func (s deploymentSleep) Asleep() bool { return s.Replicas > 0 && s.AsleepReplicas == s.Replicas }

// State is the state a fully asleep deployment shows: asleep once every engine
// reports its level, sleeping while any order is still being carried out. It
// is only meaningful when Asleep says so; callers ask for it that way.
func (s deploymentSleep) State() string {
	if s.Replicas > 0 && s.Settled == s.Replicas {
		return deploymentStateAsleep
	}
	return deploymentStateSleeping
}

// deploymentSleepFacts reads a deployment's sleep facts from the replica match
// of instances already loaded. The leader of a replica says whether it sleeps,
// as the gateway reads it: a follower sleeps with its generation's engine and
// is never judged on its own.
func deploymentSleepFacts(rows dtoLookup, view replicaView, insts []store.Instance) deploymentSleep {
	incomplete := planning.GroupsIncomplete(onlineInstances(rows, insts))
	var facts deploymentSleep
	for _, slot := range view.match.Slots {
		leader := slot.Inst
		// Only a replica that could serve counts. A replica with no instance,
		// a stopped or failed one, one whose node is gone, and a multi-node
		// generation missing ranks are all "not ready", never "asleep".
		if leader == nil || leader.State != proto.InstRunning || !instanceNodeOnline(rows, leader.NodeID) || incomplete[leader.UUID] || !followersRunning(rows, slot) {
			continue
		}
		facts.Replicas++
		if !planning.InstanceAsleep(leader) {
			continue
		}
		facts.AsleepReplicas++
		if leader.SleepLevel > 0 {
			facts.Settled++
			facts.Level = max(facts.Level, leader.SleepLevel)
		}
	}
	return facts
}

// deploymentSleepOf is deploymentSleepFacts over instances the caller already
// read; a deployment whose placement cannot be read reports no sleep facts
// rather than a guess.
func deploymentSleepOf(rows dtoLookup, d *store.Deployment, insts []store.Instance) deploymentSleep {
	view, err := newReplicaView(rows, d, insts)
	if err != nil {
		return deploymentSleep{}
	}
	return deploymentSleepFacts(rows, view, insts)
}

// followersRunning reports every follower rank of the replica running on an
// online node, as readyReplicas requires: a generation serves as a whole.
func followersRunning(rows dtoLookup, slot planning.UnitSlot) bool {
	for _, follower := range slot.Followers {
		if follower == nil || follower.State != proto.InstRunning || !instanceNodeOnline(rows, follower.NodeID) {
			return false
		}
	}
	return true
}
