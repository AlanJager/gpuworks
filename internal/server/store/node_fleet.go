package store

import (
	"context"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// NodeFleetTopic is the bus topic of the fleet changes a scheduling holder
// decides on: a node inserted, a node deleted, and a status report that
// changed the node's hold facts (NodeHoldFacts: its card count or vendor;
// its id, UUID, enrollment time and Kubernetes cluster never change). Each
// event names the node by its id, which is never used again once the node
// is deleted. Nothing else is published on it: a routine status report
// that changed no fact is not.
//
// Read events with NodeFleetChangeOf. Like every bus topic, a subscriber
// that falls behind gets each node's latest event only (an insert followed
// by a facts change reads as the facts change, anything followed by the
// delete as the delete), so a subscriber treats every event as "this node,
// and so the fleet, changed": it recomputes what it derives from NodeFleet,
// coalesced, rather than counting events.
const NodeFleetTopic = "node_fleet"

// NodeFleetChangeKind is what happened to a node of the fleet.
type NodeFleetChangeKind string

const (
	// NodeInserted: the node row was created (a pending node, a
	// registration or a Kubernetes node's enrollment).
	NodeInserted NodeFleetChangeKind = "inserted"
	// NodeDeleted: the node row was deleted; its id is never used again.
	NodeDeleted NodeFleetChangeKind = "deleted"
	// NodeFactsChanged: a status report changed the node's hold facts.
	NodeFactsChanged NodeFleetChangeKind = "facts"
)

// NodeFleetChange is one event of NodeFleetTopic.
type NodeFleetChange struct {
	NodeID uint
	Kind   NodeFleetChangeKind
}

// NodeFleetChangeOf reads a NodeFleetTopic event; ok is false for an event
// of any other topic, or one it cannot read.
func NodeFleetChangeOf(ev bus.Event) (c NodeFleetChange, ok bool) {
	if ev.Topic != NodeFleetTopic {
		return NodeFleetChange{}, false
	}
	switch kind := NodeFleetChangeKind(ev.Reason); kind {
	case NodeInserted, NodeDeleted, NodeFactsChanged:
		id, err := strconv.ParseUint(ev.Key, 10, 0)
		if err != nil || id == 0 {
			return NodeFleetChange{}, false
		}
		return NodeFleetChange{NodeID: uint(id), Kind: kind}, true
	}
	return NodeFleetChange{}, false
}

// publishNodeFleet announces a fleet change after its commit. Inserts and
// deletes are rare and a status report publishes only when it changed a
// hold fact, so the routine report costs nothing more.
func (s *Store) publishNodeFleet(id uint, kind NodeFleetChangeKind) {
	if s.Bus == nil || id == 0 {
		return
	}
	s.Bus.Publish(bus.Event{Topic: NodeFleetTopic, Key: strconv.FormatUint(uint64(id), 10), Reason: string(kind)})
}

// holdFactColumns are the node columns NodeHoldFacts is read from.
var holdFactColumns = []string{"id", "uuid", "created_at", "gpu_count", "gpu_vendor", "kube_cluster_id"}

// NodeFleet is every node's hold facts, ordered by id (enrollment order),
// in one query on the read pool. It is for a holder that derives its
// answers from the whole fleet: at boot, and for a recompute after
// NodeFleetTopic events, coalesced; never per status report or per event,
// and never from inside a hook (SchedulingHolder.Hold, a
// NodeEnrollmentGuard).
func (s *Store) NodeFleet(ctx context.Context) ([]NodeHoldFacts, error) {
	var rows []Node
	if err := s.reader().WithContext(ctx).Select(holdFactColumns).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]NodeHoldFacts, len(rows))
	for i := range rows {
		out[i] = nodeHoldFacts(&rows[i])
	}
	return out, nil
}
