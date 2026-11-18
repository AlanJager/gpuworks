package pushstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/holdpass"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Persistence is where pushed state is kept; *store.Store satisfies it.
type Persistence interface {
	LoadPushedState(ctx context.Context) (pstate.Snapshot, error)
	WritePushedState(ctx context.Context, b *pstate.Batch) (pstate.Result, error)
}

// FleetReader reads every node once, ordered by id, on the read pool: for
// the capacity selection at boot and after coalesced fleet changes, never
// per event.
type FleetReader func(ctx context.Context) ([]fleetcap.Node, error)

// Effects are what pushed state asks of the rest of the server when it
// changes. Each must return at once; nil ones are skipped.
type Effects struct {
	// ReapplyHolds asks for a pass of the scheduling holds over nodes, the
	// nodes whose answer changed, or over the whole fleet when nodes is nil
	// (coalesced by the caller, at least two seconds apart). A push that
	// changes a few holds names those nodes, so the pass reads and writes
	// them alone; only a change of the caps asks for the fleet.
	ReapplyHolds func(nodes []uint)
	// FeaturesChanged announces that feature availability or banners may
	// have changed.
	FeaturesChanged func()
	// OverrideSuperseded reports an override the owner's new push
	// replaced, for the audit log and the administrator.
	OverrideSuperseded func(o Override)
	// ClockCorrected reports a correction of extclock, for the audit log.
	ClockCorrected func(from, to time.Time, by string)
}

// Config builds a State.
type Config struct {
	Store Persistence
	// Fleet reads the fleet for the capacity selection; with nil no node is
	// ever held for capacity.
	Fleet FleetReader
	// Bus and FleetTopic wake the capacity selection on fleet changes
	// (store.NodeFleetTopic); with a nil Bus only pushes and timelines do.
	Bus        *bus.Bus
	FleetTopic string
	// Packages are the installed packages at boot.
	Packages []Package
	// MaxDownGrace is the site's ceiling on any owner's down grace; zero
	// leaves the owners' own (ADR 0071 decision 9: a site may only lower
	// it).
	MaxDownGrace time.Duration
	// CoreVersion is the server version, recorded with the version markers.
	CoreVersion string
	Clock       Clock
	Effects     Effects
	Log         *slog.Logger
	// TickInterval is the least time between two writer ticks; zero is one
	// second.
	TickInterval time.Duration
	// SelectFloor is the least time between two capacity selections; zero
	// is two seconds.
	SelectFloor time.Duration
}

// Defaults of the time rules (ADR 0071 decision 9).
const (
	DefaultDownGrace  = time.Hour
	MinDownGrace      = extapi.MinDownGraceS * time.Second
	MaxDownGrace      = extapi.MaxDownGraceS * time.Second
	BootExtension     = 2 * time.Minute
	downBannerAfter   = time.Minute
	clockBannerFor    = 24 * time.Hour
	hwmPersistEvery   = time.Minute
	defaultTick       = time.Second
	defaultSelectWait = 2 * time.Second
	maxTimerWait      = time.Minute
)

type ownerKind struct{ owner, kind string }

// State is the pushed state of one server.
type State struct {
	cfg   Config
	log   *slog.Logger
	cur   atomic.Pointer[Combined]
	clock *extclock
	wake  chan struct{}
	sel   *holdpass.Runner

	mu        sync.Mutex
	reg       *registry
	parts     map[ownerKind]*part
	pending   map[ownerKind]*pendingPush
	inflight  map[ownerKind]uint64
	ops       []*op
	spawn     map[string]uint64
	down      map[string]int64
	downDirty map[string]bool
	overrides map[int64]*Override
	corrected *clockCorrection
	hwmSaved  int64
	hwmDirty  bool
	markers   []pstate.Marker
	next      int64
	// ready is, per owner, the process generation that declared it had
	// finished starting ($/ready): a down period of that generation gets no
	// boot extension. It is not persisted, so a restarted server computes
	// the extension again from the persisted down start.
	ready map[string]uint64
	// holdsDirty marks the explicit holds for recombination: a commit, an
	// installed package set or a load changed what they are built from.
	holdsDirty bool

	// The capacity selection's own state, touched only by its pass.
	fleet      []fleetcap.Node
	fleetDirty atomic.Bool
	heldByCap  map[uint]bool
}

type clockCorrection struct {
	fromMS, toMS int64
	by           string
}

// Markers are the version markers of the canonical form of each kind's
// stored rows (extapi.Marker): a core that finds one newer than it knows
// refuses to start rather than drop or rewrite what a newer core wrote.
func Markers() map[string]int {
	out := map[string]int{}
	for _, k := range extapi.TableKinds() {
		out["pstate."+k] = 1
	}
	return out
}

// New builds a State with nothing loaded: every key absent, nothing held.
// Load reads what was persisted.
func New(cfg Config) *State {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTick
	}
	if cfg.SelectFloor <= 0 {
		cfg.SelectFloor = defaultSelectWait
	}
	s := &State{
		cfg: cfg, log: cfg.Log, wake: make(chan struct{}, 1),
		reg: newRegistry(cfg.Packages), parts: map[ownerKind]*part{}, pending: map[ownerKind]*pendingPush{}, inflight: map[ownerKind]uint64{},
		spawn: map[string]uint64{}, ready: map[string]uint64{}, down: map[string]int64{}, downDirty: map[string]bool{},
		overrides: map[int64]*Override{}, heldByCap: map[uint]bool{},
	}
	s.clock = newExtclock(cfg.Clock, 0)
	s.sel = holdpass.New(s.selectPass, cfg.Log.With("component", "capacity_selection"), cfg.SelectFloor)
	s.mu.Lock()
	s.recombineLocked()
	s.mu.Unlock()
	return s
}

// ErrCorruptRestriction is wrapped by Load when a stored restriction (a
// fleet capacity or node holds table) does not decode: skipping it would
// loosen what its owner restricted, so the server does not start until an
// administrator clears it.
var ErrCorruptRestriction = errors.New("a stored restriction does not decode")

// Load reads every persisted part, checks the version markers, places the
// fleet for capacity and publishes the snapshot. It runs once at boot,
// before the router is built and before any plugin starts, so what was
// pushed is enforced from the first request. Owners that hold state and
// have no down record are recorded as down from now: the supervisor marks
// each up when it runs, and a core that crashes before it does never renews
// their grace.
func (s *State) Load(ctx context.Context) error {
	snap, err := s.cfg.Store.LoadPushedState(ctx)
	if err != nil {
		return fmt.Errorf("load pushed state: %w", err)
	}
	stored := make([]extapi.Marker, len(snap.Markers))
	for i, m := range snap.Markers {
		stored[i] = extapi.Marker{Name: m.Name, Version: m.Version, WrittenBy: m.WrittenBy}
	}
	if err := extapi.CheckMarkers(stored, Markers()); err != nil {
		return err
	}
	tables, err := tablesOf(&snap)
	if err != nil {
		return fmt.Errorf("load pushed state: %w", err)
	}
	var hwm int64
	if snap.Clock != nil {
		hwm = snap.Clock.HWMMS
	}
	s.clock.start(hwm)
	now := s.clock.nowMS()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.hwmSaved = hwm
	for _, o := range snap.Owners {
		k := ownerKind{o.Owner, o.Kind}
		t := tables[k]
		if err := t.Validate(); err != nil {
			if extapi.ClassOf(o.Kind) == extapi.Restrict {
				return fmt.Errorf("%w: %s of %s: %w", ErrCorruptRestriction, o.Kind, o.Owner, err)
			}
			s.log.Warn("stored pushed state does not decode; it is left out", "owner", o.Owner, "kind", o.Kind, "err", err)
			continue
		}
		p := &part{owner: o.Owner, kind: o.Kind, schema: o.Schema, gen: uint64(o.Generation), digest: extapi.Digest(o.Digest), pushedAt: o.PushedAtMS, table: t}
		compile(p)
		s.parts[k] = p
		if installed, _ := s.reg.installed(o.Owner); !installed {
			s.log.Warn("pushed state of a package that is not installed is left out", "owner", o.Owner, "kind", o.Kind)
		}
	}
	for _, d := range snap.Down {
		s.down[d.Owner] = d.SinceMS
	}
	for k := range s.parts {
		if _, ok := s.down[k.owner]; !ok {
			s.down[k.owner] = now
			s.downDirty[k.owner] = true
		}
	}
	for i := range snap.Overrides {
		o := overrideOf(&snap.Overrides[i])
		s.overrides[o.ID] = &o
	}
	have := map[string]bool{}
	for _, m := range snap.Markers {
		have[m.Name] = true
	}
	for name, v := range Markers() {
		if !have[name] {
			s.markers = append(s.markers, pstate.Marker{Name: name, Version: v, WrittenBy: s.cfg.CoreVersion})
		}
	}
	if err := s.flushBootLocked(ctx); err != nil {
		return err
	}
	s.holdsDirty = true
	s.recombineLocked()
	if s.cur.Load().caps.Limited() && s.cfg.Fleet != nil {
		fleet, err := s.cfg.Fleet(ctx)
		if err != nil {
			return fmt.Errorf("load pushed state: read the fleet: %w", err)
		}
		s.fleet = fleet
		s.placeLocked()
	}
	return nil
}

// flushBootLocked persists what Load decided: down records and markers.
func (s *State) flushBootLocked(ctx context.Context) error {
	b := &pstate.Batch{Markers: s.markers}
	for owner := range s.downDirty {
		b.Down = append(b.Down, pstate.Down{Owner: owner, SinceMS: s.down[owner]})
	}
	if b.Empty() {
		return nil
	}
	if _, err := s.cfg.Store.WritePushedState(ctx, b); err != nil {
		return fmt.Errorf("record pushed state owners down: %w", err)
	}
	s.markers, s.downDirty = nil, map[string]bool{}
	return nil
}

// SetPackages replaces the installed packages (after an install, upgrade,
// verification change or uninstall) and recombines. Rows of a package no
// longer listed stay stored but are left out until Uninstall removes them.
func (s *State) SetPackages(pkgs []Package) {
	s.mu.Lock()
	s.reg = newRegistry(pkgs)
	s.holdsDirty = true
	s.changed()
}

// Held is the state the core holds for owner, per kind: what the
// handshake hands a plugin so it skips pushes the core already has.
func (s *State) Held(owner string) []extapi.HeldState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []extapi.HeldState
	for _, kind := range extapi.TableKinds() {
		if p := s.parts[ownerKind{owner, kind}]; p != nil {
			out = append(out, extapi.HeldState{Kind: kind, Schema: p.schema, Generation: p.gen, Digest: p.digest})
		}
	}
	return out
}

// Now is extclock now.
func (s *State) Now() time.Time { return time.UnixMilli(s.clock.nowMS()).UTC() }

// OwnedAttestation is one attestation and the package that pushed it.
type OwnedAttestation struct {
	Owner string
	extapi.Attestation
}

// Attestations are the attestations pushed by installed packages of
// publisher, for relaying to that publisher's packages; never interpreted.
func (s *State) Attestations(publisher string) []OwnedAttestation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OwnedAttestation
	for k, p := range s.parts {
		pkg, ok := s.reg.pkgs[k.owner]
		t, isAtt := p.table.(*extapi.AttestationsV1)
		if !ok || !isAtt || pkg.Publisher != publisher || !pkg.Verified {
			continue
		}
		for _, a := range t.Attestations {
			out = append(out, OwnedAttestation{Owner: k.owner, Attestation: a})
		}
	}
	sortAttestations(out)
	return out
}

func nodeKey(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// effects runs the hooks a recombination calls for, without blocking:
// a pass of the scheduling holds over the nodes in holds, or over the fleet
// when allHolds is set.
func (s *State) effects(holds []uint, allHolds, features bool) {
	if f := s.cfg.Effects.ReapplyHolds; f != nil {
		switch {
		case allHolds:
			f(nil)
		case len(holds) > 0:
			f(holds)
		}
	}
	if features && s.cfg.Effects.FeaturesChanged != nil {
		s.cfg.Effects.FeaturesChanged()
	}
}
