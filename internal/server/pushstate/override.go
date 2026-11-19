package pushstate

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Override actions.
const (
	OverrideDelete = pstate.OverrideDelete
	OverrideLoosen = pstate.OverrideLoosen
)

// Override is an administrator's release of one restriction entry while its
// owner cannot (ADR 0071 decision 10): a node hold deleted, a fleet cap
// raised or removed. It only loosens, is bound to the owner's generation of
// the kind, and is superseded when the owner pushes that entry again.
type Override struct {
	ID         int64
	Owner      string
	Kind       string
	Entry      string
	Generation uint64
	Action     string
	// Value is the raised cap of a loosen.
	Value  *int64
	ByUser uint
	At     time.Time
	Reason string
}

// OverrideRequest asks for an override. Entry is a node id in decimal for
// node_holds, EntryMaxNodesWithCards or EntryMaxCards for fleet_capacity.
type OverrideRequest struct {
	Owner  string
	Kind   string
	Entry  string
	Action string
	Value  *int64
	Reason string
	ByUser uint
}

func overrideOf(r *pstate.Override) Override {
	return Override{ID: r.ID, Owner: r.Owner, Kind: r.Kind, Entry: r.EntryKey, Generation: uint64(r.Generation), Action: r.Action,
		Value: copyInt(r.Value), ByUser: r.ByUser, At: time.UnixMilli(r.AtMS).UTC(), Reason: r.Reason}
}

func overrideRejected(req OverrideRequest, rule, format string) *messages.ProductError {
	return messages.NewProductError(messages.ErrorPluginOverrideRejected, format,
		map[string]any{"plugin": req.Owner, "kind": req.Kind, "entry": req.Entry, "rule": rule})
}

// Override releases one restriction entry and returns the override as
// persisted. A refusal is a *messages.ProductError (PLUGIN_OVERRIDE_REJECTED,
// with rule not_restrict, no_entry, action, not_looser or superseded); a
// persistence failure is returned as it is. The release takes effect after
// the commit and a pass of the scheduling holds.
//
// The request is checked again in the tick that commits it: an owner's
// push that changed the entry since the first check, committed or pending
// in that tick, would supersede the override at once, so it is refused
// with rule superseded instead of being stored against a decision the owner
// no longer holds. An override of a table cleared in the same tick is
// refused the same way.
func (s *State) Override(ctx context.Context, req OverrideRequest) (Override, error) {
	s.mu.Lock()
	o, perr := s.checkOverrideLocked(req)
	var want any
	if perr == nil {
		want = entryValue(s.parts[ownerKind{req.Owner, req.Kind}].table, req.Entry)
	}
	s.mu.Unlock()
	if perr != nil {
		return Override{}, perr
	}
	idx := -1
	err := s.enqueue(ctx, &op{
		build: func(b *pstate.Batch) error {
			k := ownerKind{req.Owner, req.Kind}
			for _, c := range b.Clear {
				if c.Owner == req.Owner && c.Kind == req.Kind {
					return overrideRejected(req, "superseded", "the table is cleared in the same commit")
				}
			}
			p := s.parts[k]
			if p == nil || !reflect.DeepEqual(entryValue(p.table, req.Entry), want) {
				return overrideRejected(req, "superseded", "the plugin pushed that entry again")
			}
			if pp := s.pending[k]; pp != nil && !reflect.DeepEqual(entryValue(pp.part.table, req.Entry), want) {
				return overrideRejected(req, "superseded", "the plugin pushed that entry again")
			}
			// Bind to the generation held now: a push since the first
			// check that left the entry alone does not supersede it.
			again, perr := s.checkOverrideLocked(req)
			if perr != nil {
				return perr
			}
			o.Generation = again.Generation
			// The override it replaces goes in the same commit, so a
			// restart never finds both.
			for _, other := range s.overrides {
				if other.Owner == o.Owner && other.Kind == o.Kind && other.Entry == o.Entry {
					b.DropOverrides = append(b.DropOverrides, other.ID)
				}
			}
			idx = len(b.Overrides)
			b.Overrides = append(b.Overrides, pstate.Override{Owner: o.Owner, Kind: o.Kind, EntryKey: o.Entry, Generation: int64(o.Generation),
				Action: o.Action, Value: copyInt(o.Value), ByUser: o.ByUser, AtMS: o.At.UnixMilli(), Reason: o.Reason})
			return nil
		},
		after: func(res pstate.Result, err error) {
			if err != nil {
				return
			}
			if idx < 0 || idx >= len(res.OverrideIDs) {
				return
			}
			o.ID = res.OverrideIDs[idx]
			stored := o
			s.overrides[o.ID] = &stored
		},
	})
	if err != nil {
		return Override{}, err
	}
	return o, nil
}

// RemoveOverride withdraws override id, so the owner's entry is enforced
// again as the owner last pushed it. An id that is not in force is
// PLUGIN_OVERRIDE_NOT_FOUND; a persistence failure is returned as it is and
// leaves the override in force. The hold is reapplied after the commit. The
// id is looked up again in the tick that commits the removal, so one a push
// superseded meanwhile is PLUGIN_OVERRIDE_NOT_FOUND, not a removal.
func (s *State) RemoveOverride(ctx context.Context, id int64) (Override, error) {
	s.mu.Lock()
	o, ok := s.overrides[id]
	var removed Override
	if ok {
		removed = *o
	}
	s.mu.Unlock()
	if !ok {
		return Override{}, OverrideNotFound(id)
	}
	err := s.enqueue(ctx, &op{
		build: func(b *pstate.Batch) error {
			if _, ok := s.overrides[id]; !ok {
				return OverrideNotFound(id)
			}
			b.DropOverrides = append(b.DropOverrides, id)
			return nil
		},
		after: func(pstate.Result, error) {},
	})
	if err != nil {
		return Override{}, err
	}
	return removed, nil
}

// OverrideNotFound is the refusal of an override id that is not in force.
func OverrideNotFound(id int64) *messages.ProductError {
	return messages.NewProductError(messages.ErrorPluginOverrideNotFound, fmt.Sprintf("override %d does not exist", id),
		map[string]any{"id": id})
}

// checkOverrideLocked validates req against the owner's held table.
func (s *State) checkOverrideLocked(req OverrideRequest) (Override, *messages.ProductError) {
	if extapi.ClassOf(req.Kind) != extapi.Restrict {
		return Override{}, overrideRejected(req, "not_restrict", "only a restriction can be overridden")
	}
	p := s.parts[ownerKind{req.Owner, req.Kind}]
	if p == nil {
		return Override{}, overrideRejected(req, "no_entry", "the plugin holds no such table")
	}
	o := Override{Owner: req.Owner, Kind: req.Kind, Entry: req.Entry, Generation: p.gen, Action: req.Action,
		Value: copyInt(req.Value), ByUser: req.ByUser, At: time.UnixMilli(s.clock.nowMS()).UTC(), Reason: req.Reason}
	switch t := p.table.(type) {
	case *extapi.NodeHoldsV1:
		if _, ok := t.Nodes[req.Entry]; !ok {
			return Override{}, overrideRejected(req, "no_entry", "the plugin does not hold that node")
		}
		if req.Action != OverrideDelete || req.Value != nil {
			return Override{}, overrideRejected(req, "action", "a node hold can only be deleted")
		}
	case *extapi.FleetCapacityV1:
		if req.Entry != EntryMaxNodesWithCards && req.Entry != EntryMaxCards {
			return Override{}, overrideRejected(req, "no_entry", "a fleet capacity entry is one of its caps")
		}
		i := stepAt(len(p.capacity.steps), func(i int) int64 { return p.capacity.steps[i].at }, s.clock.nowMS())
		if i < 0 {
			return Override{}, overrideRejected(req, "no_entry", "no cap is in force yet")
		}
		cap := p.capacity.steps[i].caps.Cards
		if req.Entry == EntryMaxNodesWithCards {
			cap = p.capacity.steps[i].caps.NodesWithCards
		}
		switch {
		case !cap.Limited:
			return Override{}, overrideRejected(req, "no_entry", "that cap is not set")
		case req.Action == OverrideDelete && req.Value == nil:
		case req.Action == OverrideLoosen && req.Value != nil:
			if *req.Value <= cap.Max {
				return Override{}, overrideRejected(req, "not_looser", "an override only raises a cap")
			}
		default:
			return Override{}, overrideRejected(req, "action", "delete takes no value; loosen takes the raised cap")
		}
	}
	return o, nil
}

// Overrides are the overrides in force, by id.
func (s *State) Overrides() []Override {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Override, 0, len(s.overrides))
	for _, o := range s.overrides {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortAttestations(list []OwnedAttestation) {
	slices.SortFunc(list, func(a, b OwnedAttestation) int {
		if c := strings.Compare(a.Owner, b.Owner); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}
