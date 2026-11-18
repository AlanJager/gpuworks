package pushstate

import (
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// HoldSource is the scheduling holder source of every hold pushed state
// places: one source for all plugins, so the store records "plugins" on a
// held node and the hold's code says why.
const HoldSource = "plugins"

// Hold is the pushed state's answer for one node. Its fields are those of
// store.NodeHold, so a caller converts it directly; the zero value does not
// hold the node.
type Hold struct {
	Code     string
	Fallback string
	Params   map[string]any
}

// Banner is one notice above every console page.
type Banner struct {
	// ID is "<owner>.<banner id>" for a pushed banner.
	ID string
	// Severity is info, warning or danger.
	Severity string
	Notice   *messages.ProductError
	Link     string
}

// Combined is the one snapshot the hooks read: every owner's parts combined
// at one extclock instant. It is built whole and never modified after it is
// published.
type Combined struct {
	avail  map[featuregate.Key]featuregate.Availability
	absent featuregate.Availability

	caps       fleetcap.Caps
	refuseNode *messages.ProductError
	refuseCard *messages.ProductError
	capHold    Hold

	holds    map[uint]Hold
	capacity *capacitySnap

	bannersAll   []Banner
	bannersAdmin []Banner
}

// capacitySnap is the core capacity selection's last outcome: the caps it
// used and every node it placed, so a node it has not seen yet is judged
// against the room left.
type capacitySnap struct {
	caps  fleetcap.Caps
	nodes map[uint]capacityNode
	kept  fleetcap.Usage
	full  bool
	hold  Hold
}

type capacityNode struct {
	cards int64
	held  bool
}

// answer is the capacity hold on a node with cards cards. A node the
// selection placed with the same cards keeps its place. Otherwise (a node
// not placed yet, or one whose cards changed) it is held only if counting
// it would take the fleet past a cap; the selection, woken by the fleet
// change, places it properly within the floor.
func (c *capacitySnap) answer(id uint, cards int64) Hold {
	if c == nil || !c.caps.Limited() || cards <= 0 {
		return Hold{}
	}
	n, known := c.nodes[id]
	switch {
	case known && n.held:
		return c.hold
	case known && n.cards == cards:
		return Hold{}
	case known && n.cards > 0:
		if cards < n.cards || c.caps.Fits(fleetcap.Usage{Nodes: c.kept.Nodes, Cards: c.kept.Cards + cards - n.cards}) {
			return Hold{}
		}
		return c.hold
	}
	if c.full || !c.caps.Fits(c.kept.Plus(cards)) {
		return c.hold
	}
	return Hold{}
}

// Availability answers featuregate.Source: one atomic load and one map
// lookup. A key no installed package answers for is locked with
// FEATURE_PROVIDER_ABSENT (hidden where the key is concealed).
func (s *State) Availability(k featuregate.Key) featuregate.Availability {
	c := s.cur.Load()
	if av, ok := c.avail[k]; ok {
		return av
	}
	return c.absent
}

// GuardNode answers a node insert (store.NodeEnrollmentGuard) from the
// fleet before the node joins: refused while the counted nodes or cards
// already fill a cap. A node that joins without cards is counted when they
// are reported, and held if it then exceeds a cap.
func (s *State) GuardNode(nodesWithCards, ownedCards int) *messages.ProductError {
	c := s.cur.Load()
	if c.caps.NodesWithCards.Limited && int64(nodesWithCards) >= c.caps.NodesWithCards.Max {
		return c.refuseNode
	}
	if c.caps.Cards.Limited && int64(ownedCards) >= c.caps.Cards.Max {
		return c.refuseCard
	}
	return nil
}

// Hold answers the scheduling holder for one node (store.SchedulingHolder
// under HoldSource): an explicit hold first, then fleet capacity. It runs in
// the writer's transaction on every status report and never requests a
// recompute; the capacity selection is woken by the fleet topic instead.
func (s *State) Hold(id uint, ownedCards int) Hold {
	c := s.cur.Load()
	if h, ok := c.holds[id]; ok {
		return h
	}
	return c.capacity.answer(id, int64(ownedCards))
}

// Banners are the notices above every console page for a viewer: all of
// them for an administrator, those meant for everyone otherwise.
func (s *State) Banners(admin bool) []Banner {
	c := s.cur.Load()
	if admin {
		return c.bannersAdmin
	}
	return c.bannersAll
}
