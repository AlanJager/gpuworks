package pushstate

import (
	"context"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/fleetcap"
)

// fleetReadTimeout bounds one read of the fleet by the capacity selection.
const fleetReadTimeout = 30 * time.Second

// selectPass is one pass of the core capacity selection (a holdpass apply,
// so passes are coalesced and at least SelectFloor apart): it reads the
// fleet again only after a fleet change and only while a cap is set, fits
// it into the combined caps (kept nodes first, then enrollment order),
// publishes the outcome and asks for a pass of the scheduling holds over
// the nodes it held or released, or over the fleet when the caps or kept
// nodes changed. It never runs inside a hook.
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
	var prevKeep []uint
	if c := s.cur.Load().capacity; c != nil {
		prev, prevKeep = c.caps, c.keep
	}
	changed := s.placeLocked()
	for _, id := range changed {
		if s.heldByCap[id] {
			held = append(held, nodeKey(id))
		} else {
			released = append(released, nodeKey(id))
		}
	}
	now := s.cur.Load().capacity
	capsChanged := now.caps != prev || !slices.Equal(now.keep, prevKeep)
	s.mu.Unlock()
	// A changed cap or kept set changes the answer for nodes the selection
	// has not placed yet too (Hold judges them against the room left), so
	// it asks for a pass over the fleet even when no placed node changed.
	s.effects(changed, capsChanged, false)
	return held, released, nil
}

// placeLocked fits the fleet last read into the current caps and publishes
// the outcome in the snapshot. It returns the nodes newly held or released
// since the last placement, ascending.
func (s *State) placeLocked() (changed []uint) {
	c := s.cur.Load()
	snap := &capacitySnap{caps: c.caps, keep: c.keep, hold: c.capHold}
	next := map[uint]bool{}
	if c.caps.Limited() {
		sel := fleetcap.Select(c.caps, s.fleet, c.keep)
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
			changed = append(changed, id)
		}
	}
	for id := range s.heldByCap {
		if !next[id] {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)
	s.heldByCap = next
	nc := *c
	nc.capacity = snap
	s.cur.Store(&nc)
	return changed
}
