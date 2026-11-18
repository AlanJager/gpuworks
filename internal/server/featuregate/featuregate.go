// Package featuregate answers whether an optional capability that a
// separately built server adds is available right now, and how far: fully,
// read-only, shown but locked, or hidden. It speaks availability only; who
// decides it and why is the Source's business, and this package interprets no
// key.
//
// Only extension routes, extension console entries and guards an extension
// registers name a feature key. Community code paths never consult a Gate.
//
// The Gate is read on hot paths (every request to an extension route), so a
// Source answers from a snapshot it swaps atomically: one atomic load and one
// map lookup, no I/O, no lock and no allocation when the answer is Full. The
// refusals a Gate returns are built once, when the Gate is (for a
// dynamic declaration, when the DynamicSet takes it).
package featuregate

import (
	"fmt"
	"regexp"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Key names one feature: up to three dot-separated lowercase segments, for
// example "audit" or "iam.custom_roles".
type Key string

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}(\.[a-z][a-z0-9_]{1,31}){0,2}$`)

// Valid reports whether k follows the key grammar.
func (k Key) Valid() bool { return keyPattern.MatchString(string(k)) }

// Access is how far a feature is available. The zero value is Hidden, so an
// unset answer refuses.
type Access uint8

const (
	// Hidden: the feature does not exist for this server; its routes answer
	// 404 and the console shows nothing of it.
	Hidden Access = iota
	// Locked: the console shows the feature, but nothing of it works; its
	// routes answer 403.
	Locked
	// ReadOnly: reads work and writes are refused, except the writes a route
	// declares safe while read-only.
	ReadOnly
	// Full: everything works.
	Full
)

// String is the wire name of a: hidden, locked, read_only or full.
func (a Access) String() string {
	switch a {
	case Locked:
		return "locked"
	case ReadOnly:
		return "read_only"
	case Full:
		return "full"
	}
	return "hidden"
}

// Presentation is what the console shows of a feature that is not available.
type Presentation uint8

const (
	// Promote shows an unavailable feature as locked, with the Source's link.
	Promote Presentation = iota
	// Conceal hides an unavailable feature entirely: a Locked answer counts
	// as Hidden.
	Conceal
)

// String is the wire name of p: promote or hidden.
func (p Presentation) String() string {
	if p == Conceal {
		return "hidden"
	}
	return "promote"
}

// Feature declares one feature a Gate answers for.
type Feature struct {
	Key          Key
	Presentation Presentation
}

// Limit is one quantity a feature is available up to. Dim is opaque to this
// package (a count of cards, of concurrent jobs, ...).
type Limit struct {
	Dim       string
	Max       int64
	Unlimited bool
}

// Availability is a Source's answer for one key.
type Availability struct {
	Access Access
	// Notice is the refusal to return instead of the Gate's own, built by
	// the Source when its snapshot changed; nil uses the Gate's.
	Notice *messages.ProductError
	// Link is where the console sends a user about an unavailable feature;
	// empty for none.
	Link string
	// Limits are sorted by Dim. A dimension not listed is not limited.
	Limits []Limit
}

// Source answers availability from a snapshot: one atomic load and one map
// lookup, with no I/O and no allocation.
type Source interface {
	Availability(k Key) Availability
}

// Gate answers for the features declared to it: the static ones it was
// built with, then those its DynamicSet holds now. A nil *Gate declares
// none.
type Gate struct {
	src      Source
	features map[Key]*feature
	order    []Key
	dynamic  *DynamicSet
}

type feature struct {
	spec        Feature
	unavailable *messages.ProductError
	readOnly    *messages.ProductError
	absent      *messages.ProductError
}

// New builds a Gate for features over src. A nil src fails closed: every
// declared feature is unavailable (Locked, or Hidden when concealed).
func New(features []Feature, src Source) (*Gate, error) {
	byKey, order, err := declare(features)
	if err != nil {
		return nil, err
	}
	return &Gate{src: src, features: byKey, order: order}, nil
}

// NewWithDynamic builds a Gate for the static features over src that also
// answers for the features d holds at each lookup, over the Source each
// was declared with (DynamicSet.Replace). d must have been made over the
// same static features (NewDynamicSet), so no key is ever declared by
// both. A nil d is New.
func NewWithDynamic(static []Feature, src Source, d *DynamicSet) (*Gate, error) {
	g, err := New(static, src)
	if err != nil || d == nil {
		return g, err
	}
	for _, k := range g.order {
		if _, ok := d.reserved[k]; !ok {
			return nil, fmt.Errorf("feature %q was declared after the dynamic set was made", k)
		}
	}
	g.dynamic = d
	return g, nil
}

// declare checks features and builds each one's refusals, once.
func declare(features []Feature) (map[Key]*feature, []Key, error) {
	byKey := make(map[Key]*feature, len(features))
	order := make([]Key, 0, len(features))
	for _, f := range features {
		if !f.Key.Valid() {
			return nil, nil, fmt.Errorf("feature key %q is not valid", f.Key)
		}
		if f.Presentation != Promote && f.Presentation != Conceal {
			return nil, nil, fmt.Errorf("feature %q: unknown presentation %d", f.Key, f.Presentation)
		}
		if _, dup := byKey[f.Key]; dup {
			return nil, nil, fmt.Errorf("feature %q declared twice", f.Key)
		}
		params := map[string]any{"feature": string(f.Key)}
		byKey[f.Key] = &feature{
			spec:        f,
			unavailable: messages.NewProductError(messages.ErrorFeatureUnavailable, "feature "+string(f.Key)+" is not available on this server", params),
			readOnly:    messages.NewProductError(messages.ErrorFeatureReadOnly, "feature "+string(f.Key)+" is read-only on this server", params),
			absent:      messages.NewProductError(messages.ErrorFeatureProviderAbsent, "nothing on this server answers for feature "+string(f.Key), params),
		}
		order = append(order, f.Key)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	return byKey, order, nil
}

// find is the declared feature k and the Source that answers for it: a
// static declaration first, then the dynamic set's current snapshot.
func (g *Gate) find(k Key) (*feature, Source, bool) {
	if f, ok := g.features[k]; ok {
		return f, g.src, true
	}
	if g.dynamic != nil {
		if s := g.dynamic.cur.Load(); s != nil {
			if f, ok := s.features[k]; ok {
				return f, s.src, true
			}
		}
	}
	return nil, nil, false
}

// Declared reports whether the Gate answers for k.
func (g *Gate) Declared(k Key) bool {
	if g == nil {
		return false
	}
	_, _, ok := g.find(k)
	return ok
}

// lookup is the declared feature and the Source's answer for it with the
// presentation applied; ok is false for an undeclared key.
func (g *Gate) lookup(k Key) (*feature, Availability, bool) {
	if g == nil {
		return nil, Availability{}, false
	}
	f, src, ok := g.find(k)
	if !ok {
		return nil, Availability{}, false
	}
	return f, answer(f, src), true
}

// answer is src's answer for f with the presentation applied; a nil src
// fails closed.
func answer(f *feature, src Source) Availability {
	var av Availability
	if src == nil {
		av = Availability{Access: Locked, Notice: f.absent}
	} else {
		av = src.Availability(f.spec.Key)
	}
	if av.Access > Full {
		av.Access = Hidden
	}
	if av.Access == Locked && f.spec.Presentation == Conceal {
		av.Access = Hidden
	}
	return av
}

// Access is k's availability now; Hidden for an undeclared key.
func (g *Gate) Access(k Key) Access {
	_, av, _ := g.lookup(k)
	return av.Access
}

// Require answers whether a request may use k: a read (write false) or a
// write. A nil error allows it; otherwise the error is the refusal to send
// and the Access says how (Hidden → not found, else forbidden). An
// undeclared key is refused as Hidden.
func (g *Gate) Require(k Key, write bool) (Access, *messages.ProductError) {
	f, av, ok := g.lookup(k)
	if !ok {
		return Hidden, undeclared(k)
	}
	switch {
	case av.Access == Full, av.Access == ReadOnly && !write:
		return av.Access, nil
	case av.Notice != nil:
		return av.Access, av.Notice
	case av.Access == ReadOnly:
		return av.Access, f.readOnly
	}
	return av.Access, f.unavailable
}

// Admit answers whether adding delta to used stays within k's limit on dim.
// It first requires k for a write; a dimension the Source does not limit
// admits anything.
func (g *Gate) Admit(k Key, dim string, used, delta int64) *messages.ProductError {
	if _, err := g.Require(k, true); err != nil {
		return err
	}
	_, av, _ := g.lookup(k)
	for _, l := range av.Limits {
		if l.Dim != dim {
			continue
		}
		if !l.Unlimited && used+delta > l.Max {
			return messages.NewProductError(messages.ErrorFeatureLimitReached,
				fmt.Sprintf("feature %s allows at most %d %s", k, l.Max, dim),
				map[string]any{"feature": string(k), "dim": dim, "max": l.Max, "used": used})
		}
		return nil
	}
	return nil
}

// State is one declared feature's availability, for the console.
type State struct {
	Key          Key
	Presentation Presentation
	Availability
}

// States lists every declared feature's availability now, static and
// dynamic together, sorted by key.
func (g *Gate) States() []State {
	if g == nil {
		return nil
	}
	var dyn *dynamicSnapshot
	if g.dynamic != nil {
		dyn = g.dynamic.cur.Load()
	}
	if dyn == nil {
		dyn = &dynamicSnapshot{}
	}
	out := make([]State, 0, len(g.order)+len(dyn.order))
	i, j := 0, 0
	for i < len(g.order) || j < len(dyn.order) {
		var f *feature
		var src Source
		if j == len(dyn.order) || (i < len(g.order) && g.order[i] < dyn.order[j]) {
			f, src = g.features[g.order[i]], g.src
			i++
		} else {
			f, src = dyn.features[dyn.order[j]], dyn.src
			j++
		}
		out = append(out, State{Key: f.spec.Key, Presentation: f.spec.Presentation, Availability: answer(f, src)})
	}
	return out
}

func undeclared(k Key) *messages.ProductError {
	return messages.NewProductError(messages.ErrorFeatureUnavailable, "feature "+string(k)+" is not available on this server", map[string]any{"feature": string(k)})
}
