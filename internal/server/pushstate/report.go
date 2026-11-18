package pushstate

import (
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// OwnerRows is owner's committed table of kind as the core holds it, for the
// owner itself to reconcile after a restart: the canonical body, its digest
// and the extclock at which the push was accepted. It is read from memory;
// another owner's table of the same kind is never in it. Nothing held, a
// push still waiting for its commit included, answers with Kind alone.
func (s *State) OwnerRows(owner, kind string) (extapi.PushedState, error) {
	s.mu.Lock()
	p := s.parts[ownerKind{owner, kind}]
	if p == nil {
		s.mu.Unlock()
		return extapi.PushedState{Kind: kind}, nil
	}
	schema, gen, digest, pushedAt, table := p.schema, p.gen, p.digest, p.pushedAt, p.table
	s.mu.Unlock()
	// A part's table is never changed once it is held, so it is encoded
	// outside the lock.
	body, err := extapi.Marshal(table)
	if err != nil {
		return extapi.PushedState{}, err
	}
	at := extapi.NewTime(time.UnixMilli(pushedAt))
	return extapi.PushedState{Kind: kind, Schema: schema, Generation: gen, Digest: digest, PushedAt: &at, Body: body}, nil
}

// ClockReading is the server's clocks as pushed state keeps them.
type ClockReading struct {
	// Wall is the wall clock and ExtClock the clock timelines follow.
	Wall, ExtClock time.Time
	// HWM is the high-water mark last persisted; zero before the first.
	HWM time.Time
}

// Clocks reads the wall clock, extclock and the persisted high-water mark.
func (s *State) Clocks() ClockReading {
	ext := s.clock.nowMS()
	r := ClockReading{Wall: s.clock.wall().UTC(), ExtClock: time.UnixMilli(ext).UTC()}
	s.mu.Lock()
	hwm := s.hwmSaved
	s.mu.Unlock()
	if hwm > 0 {
		r.HWM = time.UnixMilli(hwm).UTC()
	}
	return r
}
