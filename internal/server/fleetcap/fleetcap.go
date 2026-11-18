// Package fleetcap is the core's rule for fitting a fleet into capacity caps:
// keep nodes in the order they joined until a cap is full, and hold the
// rest from scheduling. Pushed fleet capacity (docs/reference/plugin-protocol.md,
// table.fleet_capacity) gives the caps; the core, not the pusher, selects the
// nodes, so nodes that join while the pusher is down are placed by the same
// rule.
//
// It is a pure leaf: no I/O, no clock, no store.
package fleetcap

import "sort"

// Limit is one cap. The zero value is unlimited.
type Limit struct {
	Limited bool
	Max     int64
}

// Allows reports whether n fits the cap.
func (l Limit) Allows(n int64) bool { return !l.Limited || n <= l.Max }

// Min is the tighter of two caps.
func Min(a, b Limit) Limit {
	switch {
	case !a.Limited:
		return b
	case !b.Limited:
		return a
	case b.Max < a.Max:
		return b
	}
	return a
}

// Caps are the fleet's caps: nodes that have at least one card, and cards.
type Caps struct {
	NodesWithCards Limit
	Cards          Limit
}

// Limited reports whether any cap is set.
func (c Caps) Limited() bool { return c.NodesWithCards.Limited || c.Cards.Limited }

// Fits reports whether u fits both caps.
func (c Caps) Fits(u Usage) bool {
	return c.NodesWithCards.Allows(u.Nodes) && c.Cards.Allows(u.Cards)
}

// Usage counts nodes with cards and their cards.
type Usage struct {
	Nodes int64
	Cards int64
}

// Plus is u with one more node of cards cards.
func (u Usage) Plus(cards int64) Usage { return Usage{Nodes: u.Nodes + 1, Cards: u.Cards + cards} }

// Node is one node as the rule sees it.
type Node struct {
	// ID is the node's id; ids grow in enrollment order and are never
	// reused.
	ID uint
	// Cards are the cards the node may use. A node without one is not
	// counted and never held.
	Cards int64
}

// Selection is the outcome of fitting a fleet into caps.
type Selection struct {
	// Used counts every counted node (at least one card).
	Used Usage
	// Kept counts the nodes within the caps.
	Kept Usage
	// Full reports that some counted node did not fit: every node that
	// joins later is held too, even a smaller one.
	Full bool
	// Held lists, by ascending id, the counted nodes that do not fit.
	Held []uint
}

// Select keeps counted nodes in enrollment order while both caps still fit.
// The first node that does not fit and every counted node after it are held:
// a later node never displaces an earlier one, even if it is smaller.
func Select(c Caps, nodes []Node) Selection {
	ordered := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Cards > 0 {
			ordered = append(ordered, n)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	var s Selection
	for _, n := range ordered {
		s.Used = s.Used.Plus(n.Cards)
		if next := s.Kept.Plus(n.Cards); !s.Full && c.Fits(next) {
			s.Kept = next
			continue
		}
		s.Full = true
		s.Held = append(s.Held, n.ID)
	}
	return s
}
