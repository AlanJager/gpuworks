package pushstate

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/fleetcap"
)

// fleetReadTimeout bounds one read of the fleet by the capacity selection.
const fleetReadTimeout = 30 * time.Second

// selectPass is one pass of the core capacity selection (a holdpass apply,
// so passes are coalesced and at least SelectFloor apart): it reads the
// fleet again only after a fleet change and only while a cap is set, fits
// it into the combined caps by enrollment order, publishes the outcome and
// asks for a pass of the scheduling holds when the set of held nodes
// changed. It never runs inside a hook.
func (s *State) selectPass() (held, released []string, err error) {
	if s.cfg.Fleet == nil {
		return nil, nil, nil
	}
	if s.cur.Load().caps.Limited() && s.fleetDirty.Swap(false) {
		ctx, cancel := context.WithTimeout(context.Background(), fleetReadTimeout)
		fleet, err := s.cfg.Fleet(ctx)
		cancel()
		if err != nil {
			s.fleetDirty.Store(true)
			return nil, nil, err
		}
		s.mu.Lock()
		s.fleet = fleet
		s.mu.Unlock()
	}
	s.mu.Lock()
	var prev fleetcap.Caps
	if c := s.cur.Load().capacity; c != nil {
		prev = c.caps
	}
	held, released = s.placeLocked()
	capsChanged := s.cur.Load().capacity.caps != prev
	s.mu.Unlock()
	// A changed cap changes the answer for nodes the selection has not
	// placed yet too (Hold judges them against the room left), so it asks
	// for a pass even when no placed node changed.
	if len(held) > 0 || len(released) > 0 || capsChanged {
		s.effects(true, false)
	}
	return held, released, nil
}

// placeLocked fits the fleet last read into the current caps and publishes
// the outcome in the snapshot. It returns the nodes newly held and those
// released since the last placement.
func (s *State) placeLocked() (held, released []string) {
	c := s.cur.Load()
	snap := &capacitySnap{caps: c.caps, hold: c.capHold}
	next := map[uint]bool{}
	if c.caps.Limited() {
		sel := fleetcap.Select(c.caps, s.fleet, nil)
		snap.kept, snap.full = sel.Kept, sel.Full
		for _, id := range sel.Held {
			next[id] = true
		}
		snap.nodes = make(map[uint]capacityNode, len(s.fleet))
		for _, n := range s.fleet {
			snap.nodes[n.ID] = capacityNode{cards: n.Cards, held: next[n.ID]}
		}
	}
	for id := range next {
		if !s.heldByCap[id] {
			held = append(held, nodeKey(id))
		}
	}
	for id := range s.heldByCap {
		if !next[id] {
			released = append(released, nodeKey(id))
		}
	}
	s.heldByCap = next
	nc := *c
	nc.capacity = snap
	s.cur.Store(&nc)
	return held, released
}
