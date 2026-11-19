package pushstate

import (
	"maps"
	"math"
	"slices"
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

// TableReport is one table an owner holds, as an administrator reads it.
type TableReport struct {
	Kind       string
	Schema     int
	Generation uint64
	Digest     extapi.Digest
	// Entries counts its entries (EntriesOf).
	Entries  int
	PushedAt time.Time
	// NextStep is the extclock instant at which one of its timelines next
	// takes a step; zero when none is ahead.
	NextStep time.Time
}

// OwnerReport is what pushed state holds for one owner and how it stands:
// read from memory, for the administrator (GET /api/v1/plugins/{id}/state).
type OwnerReport struct {
	Owner string
	// Installed and Verified are what the installed packages say of the
	// owner; an uninstalled owner whose rows are still held (an uninstall
	// that failed half way) is reported too.
	Installed, Verified bool
	// Tables are the tables it holds, by kind.
	Tables []TableReport
	// DownSince is when it went down, by extclock; zero while it is up.
	DownSince time.Time
	// Grace is its down grace; ClampAt is when its grants clamp, zero while
	// it is up.
	Grace   time.Duration
	ClampAt time.Time
	// Overrides are the administrators' overrides of its restrictions, by
	// id.
	Overrides []Override
	// Now is extclock now, which every instant above is on.
	Now time.Time
}

// OwnerReport reports owner's pushed state.
func (s *State) OwnerReport(owner string) OwnerReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.nowMS()
	r := OwnerReport{Owner: owner, Grace: s.graceLocked(owner), Now: time.UnixMilli(now).UTC()}
	r.Installed, r.Verified = s.reg.installed(owner)
	for _, kind := range extapi.TableKinds() {
		p := s.parts[ownerKind{owner, kind}]
		if p == nil {
			continue
		}
		t := TableReport{Kind: kind, Schema: p.schema, Generation: p.gen, Digest: p.digest, Entries: EntriesOf(p.table),
			PushedAt: time.UnixMilli(p.pushedAt).UTC()}
		if next := p.nextStep(now); next != math.MaxInt64 {
			t.NextStep = time.UnixMilli(next).UTC()
		}
		r.Tables = append(r.Tables, t)
	}
	if since, down := s.down[owner]; down {
		r.DownSince = time.UnixMilli(since).UTC()
		r.ClampAt = time.UnixMilli(s.clampAtLocked(owner)).UTC()
	}
	for _, id := range slices.Sorted(maps.Keys(s.overrides)) {
		if o := s.overrides[id]; o.Owner == owner {
			c := *o
			c.Value = copyInt(o.Value)
			r.Overrides = append(r.Overrides, c)
		}
	}
	return r
}

// nextStep is the earliest instant after now at which one of p's timelines
// takes a step, or math.MaxInt64.
func (p *part) nextStep(now int64) int64 {
	next := int64(math.MaxInt64)
	take := func(n int, at func(int) int64) { next = min(next, nextAfter(n, at, now)) }
	switch {
	case p.avail != nil:
		for _, row := range p.avail.rows {
			take(len(row.steps), func(i int) int64 { return row.steps[i].at })
		}
	case p.capacity != nil:
		take(len(p.capacity.steps), func(i int) int64 { return p.capacity.steps[i].at })
	case p.banners != nil:
		for _, b := range p.banners {
			take(len(b.steps), func(i int) int64 { return b.steps[i].at })
		}
	}
	return next
}
