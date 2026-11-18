package pushstate

import (
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// providerAbsent answers a key no installed package declares.
var providerAbsent = messages.NewProductError(messages.ErrorFeatureProviderAbsent,
	"nothing on this server answers for this feature", nil)

// Override entries of a fleet capacity table: its two caps.
const (
	EntryMaxNodesWithCards = "max_nodes_with_cards"
	EntryMaxCards          = "max_cards"
)

// graceLocked is owner's down grace: what it pushed with its availability
// table, or the default, never above the site's ceiling.
func (s *State) graceLocked(owner string) time.Duration {
	g := DefaultDownGrace
	if p := s.parts[ownerKind{owner, extapi.KindAvailability}]; p != nil && p.avail != nil && p.avail.downGraceS != 0 {
		g = time.Duration(p.avail.downGraceS) * time.Second
	}
	if m := s.cfg.MaxDownGrace; m > 0 {
		g = min(g, max(m, MinDownGrace))
	}
	return g
}

// clampAtLocked is the extclock instant from which owner's grants clamp:
// its persisted down start plus its grace plus the boot extension, all from
// the persisted start, so neither a restart nor a crash loop renews it. It
// is math.MaxInt64 while owner is up.
func (s *State) clampAtLocked(owner string) int64 {
	since, down := s.down[owner]
	if !down {
		return math.MaxInt64
	}
	return since + (s.graceLocked(owner) + BootExtension).Milliseconds()
}

// overridesLocked indexes the overrides by owner and kind, then entry.
func (s *State) overridesLocked() map[ownerKind]map[string]*Override {
	out := map[ownerKind]map[string]*Override{}
	for _, o := range s.overrides {
		k := ownerKind{o.Owner, o.Kind}
		if out[k] == nil {
			out[k] = map[string]*Override{}
		}
		out[k][o.Entry] = o
	}
	return out
}

// sortedParts are the parts of kind whose owner is installed (and, with
// verified, verifies), by owner id.
func (s *State) sortedParts(kind string, verified bool) []*part {
	var out []*part
	for k, p := range s.parts {
		installed, ok := s.reg.installed(k.owner)
		if k.kind != kind || !installed || verified && !ok {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].owner < out[j].owner })
	return out
}

// recombineLocked builds the snapshot from every part at extclock now and
// publishes it, keeping the capacity selection's last outcome. It reports
// whether the explicit holds or what the console shows changed, and asks
// the selection again when the caps changed. It also re-arms the step timer
// at the next instant anything changes on its own.
func (s *State) recombineLocked() (holdsChanged, featuresChanged bool) {
	now := s.clock.nowMS()
	next := int64(math.MaxInt64)
	soon := func(at int64) {
		if at > now && at < next {
			next = at
		}
	}
	prev := s.cur.Load()
	c := &Combined{absent: featuregate.Availability{Access: featuregate.Locked, Notice: providerAbsent}}
	overrides := s.overridesLocked()

	c.avail = s.combineAvailabilityLocked(now, soon)
	s.combineCapacityLocked(c, now, soon, overrides)
	// Explicit holds do not follow the clock: a recombination for a step,
	// a grace or an owner going down reuses them.
	if prev != nil && !s.holdsDirty {
		c.holds = prev.holds
	} else {
		c.holds, s.holdsDirty = s.combineHoldsLocked(overrides), false
	}
	c.bannersAll, c.bannersAdmin = s.combineBannersLocked(now, soon)
	if prev != nil {
		c.capacity = prev.capacity
	}
	s.cur.Store(c)
	s.next = next
	if prev == nil || prev.caps != c.caps || !sameHolds(map[uint]Hold{0: prev.capHold}, map[uint]Hold{0: c.capHold}) {
		s.sel.Request()
	}
	if prev == nil {
		return len(c.holds) > 0, true
	}
	return !sameHolds(prev.holds, c.holds), !sameAvailability(prev.avail, c.avail) || !sameBanners(prev.bannersAdmin, c.bannersAdmin)
}

func (s *State) combineAvailabilityLocked(now int64, soon func(int64)) map[featuregate.Key]featuregate.Availability {
	out := make(map[featuregate.Key]featuregate.Availability, len(s.reg.decls))
	for k, d := range s.reg.decls {
		if d.gate == extapi.GateNone {
			out[k] = featuregate.Availability{Access: featuregate.Full}
			continue
		}
		out[k] = featuregate.Availability{Access: featuregate.Locked, Notice: messages.NewProductError(messages.ErrorFeatureProviderAbsent,
			"nothing on this server answers for feature "+string(k), map[string]any{"feature": string(k)})}
	}
	for _, p := range s.sortedParts(extapi.KindAvailability, true) {
		pkg := s.reg.pkgs[p.owner]
		clampAt := s.clampAtLocked(p.owner)
		soon(clampAt)
		for _, row := range p.avail.rows {
			if !s.reg.answers(p.owner, pkg.Publisher, row.key) {
				continue
			}
			steps := row.steps
			at := func(i int) int64 { return steps[i].at }
			soon(nextAfter(len(steps), at, now))
			i := stepAt(len(steps), at, now)
			if i < 0 {
				continue
			}
			av := steps[i].av
			if now >= clampAt && av.Access > row.downAccess {
				av.Access = row.downAccess
				av.Notice = messages.NewProductError(messages.ErrorPluginUnavailable,
					"feature "+string(row.key)+" is limited because plugin "+p.owner+" has been down longer than its grace",
					map[string]any{"plugin": p.owner, "feature": string(row.key)})
			}
			out[row.key] = av
		}
	}
	return out
}

func (s *State) combineCapacityLocked(c *Combined, now int64, soon func(int64), overrides map[ownerKind]map[string]*Override) {
	var holdN, holdC Hold
	for _, p := range s.sortedParts(extapi.KindFleetCapacity, false) {
		steps := p.capacity.steps
		at := func(i int) int64 { return steps[i].at }
		soon(nextAfter(len(steps), at, now))
		i := stepAt(len(steps), at, now)
		if i < 0 {
			continue
		}
		caps := steps[i].caps
		ov := overrides[ownerKind{p.owner, extapi.KindFleetCapacity}]
		caps.NodesWithCards = loosened(caps.NodesWithCards, ov[EntryMaxNodesWithCards])
		caps.Cards = loosened(caps.Cards, ov[EntryMaxCards])
		// The tighter cap wins; on a tie the owner first by id keeps its
		// notices.
		if caps.NodesWithCards.Limited && (!c.caps.NodesWithCards.Limited || caps.NodesWithCards.Max < c.caps.NodesWithCards.Max) {
			c.caps.NodesWithCards, c.refuseNode, holdN = caps.NodesWithCards, p.capacity.refuseNode, p.capacity.hold
		}
		if caps.Cards.Limited && (!c.caps.Cards.Limited || caps.Cards.Max < c.caps.Cards.Max) {
			c.caps.Cards, c.refuseCard, holdC = caps.Cards, p.capacity.refuseCard, p.capacity.hold
		}
	}
	// A held node shows the notice of the owner whose node cap binds, else
	// of the one whose card cap does.
	c.capHold = holdC
	if c.caps.NodesWithCards.Limited {
		c.capHold = holdN
	}
}

// loosened is cap after an override of it.
func loosened(cap fleetcap.Limit, o *Override) fleetcap.Limit {
	switch {
	case o == nil || !cap.Limited:
		return cap
	case o.Action == OverrideDelete:
		return fleetcap.Limit{}
	case o.Value != nil && *o.Value > cap.Max:
		return fleetcap.Limit{Limited: true, Max: *o.Value}
	}
	return cap
}

func (s *State) combineHoldsLocked(overrides map[ownerKind]map[string]*Override) map[uint]Hold {
	out := map[uint]Hold{}
	for _, p := range s.sortedParts(extapi.KindNodeHolds, false) {
		ov := overrides[ownerKind{p.owner, extapi.KindNodeHolds}]
		for id, h := range p.holds {
			if _, released := ov[nodeKey(id)]; released {
				continue
			}
			if _, held := out[id]; !held {
				out[id] = h
			}
		}
	}
	return out
}

var severityRank = map[string]int{extapi.SeverityDanger: 0, extapi.SeverityWarning: 1, extapi.SeverityInfo: 2}

func (s *State) combineBannersLocked(now int64, soon func(int64)) (all, admin []Banner) {
	for _, p := range s.sortedParts(extapi.KindBanners, false) {
		for _, b := range p.banners {
			steps := b.steps
			at := func(i int) int64 { return steps[i].at }
			soon(nextAfter(len(steps), at, now))
			i := stepAt(len(steps), at, now)
			if i < 0 || steps[i].banner.ID == "" {
				continue
			}
			admin = append(admin, steps[i].banner)
			if !b.admin {
				all = append(all, steps[i].banner)
			}
		}
	}
	for _, owner := range slices.Sorted(maps.Keys(s.down)) {
		if !s.holdsStateLocked(owner) {
			continue
		}
		since := s.down[owner]
		if shown := since + downBannerAfter.Milliseconds(); now < shown {
			soon(shown)
			continue
		}
		admin = append(admin, Banner{ID: "plugins.down." + owner, Severity: extapi.SeverityWarning,
			Notice: messages.NewProductError(messages.ErrorPluginDown, "plugin "+owner+" is down; what it set up stays in force",
				map[string]any{"plugin": owner, "since": time.UnixMilli(since).UTC().Format(time.RFC3339)})})
	}
	if cc := s.corrected; cc != nil {
		if until := s.clockBannerUntil(); now < until {
			soon(until)
			admin = append(admin, Banner{ID: "plugins.clock_corrected", Severity: extapi.SeverityWarning,
				Notice: messages.NewProductError(messages.ErrorPluginClockCorrected, "the plugin clock was corrected",
					map[string]any{"from": time.UnixMilli(cc.fromMS).UTC().Format(time.RFC3339), "to": time.UnixMilli(cc.toMS).UTC().Format(time.RFC3339), "by": cc.by})})
		}
	}
	order := func(list []Banner) {
		sort.SliceStable(list, func(i, j int) bool {
			if a, b := severityRank[list[i].Severity], severityRank[list[j].Severity]; a != b {
				return a < b
			}
			return strings.Compare(list[i].ID, list[j].ID) < 0
		})
	}
	order(all)
	order(admin)
	return all, admin
}

func (s *State) clockBannerUntil() int64 { return s.corrected.toMS + clockBannerFor.Milliseconds() }

// holdsStateLocked reports whether owner is installed and holds any part.
func (s *State) holdsStateLocked(owner string) bool {
	if installed, _ := s.reg.installed(owner); !installed {
		return false
	}
	for k := range s.parts {
		if k.owner == owner {
			return true
		}
	}
	return false
}

func sameHolds(a, b map[uint]Hold) bool {
	if len(a) != len(b) {
		return false
	}
	for id, h := range a {
		o, ok := b[id]
		if !ok || o.Code != h.Code || !reflect.DeepEqual(o.Params, h.Params) {
			return false
		}
	}
	return true
}

func sameNotice(a, b *messages.ProductError) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Code() == b.Code() && reflect.DeepEqual(a.Params(), b.Params())
}

func sameAvailability(a, b map[featuregate.Key]featuregate.Availability) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || x.Access != y.Access || x.Link != y.Link || !slices.Equal(x.Limits, y.Limits) || !sameNotice(x.Notice, y.Notice) {
			return false
		}
	}
	return true
}

func sameBanners(a, b []Banner) bool {
	return slices.EqualFunc(a, b, func(x, y Banner) bool {
		return x.ID == y.ID && x.Severity == y.Severity && x.Link == y.Link && sameNotice(x.Notice, y.Notice)
	})
}
