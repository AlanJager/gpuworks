package pushstate

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The tables' typed rows (package pstate) and back. A kind's rows are
// replaced per entry, so a push that changes a few of many rows writes only
// those: a feature key, a node id, a banner id, an attestation name; a fleet
// capacity table is one entry.

const wholeTable = ""

// entriesOf maps each entry of t to the value compared to tell whether it
// changed.
func entriesOf(t extapi.Table) map[string]any {
	out := map[string]any{}
	switch t := t.(type) {
	case *extapi.AvailabilityV1:
		for _, r := range t.Rows {
			out[r.Feature] = r
		}
	case *extapi.FleetCapacityV1:
		out[wholeTable] = *t
	case *extapi.NodeHoldsV1:
		for id, n := range t.Nodes {
			out[id] = n
		}
	case *extapi.BannersV1:
		for _, b := range t.Banners {
			out[b.ID] = b
		}
	case *extapi.AttestationsV1:
		for _, a := range t.Attestations {
			out[a.Name] = a
		}
	}
	return out
}

// tableWrite is the write that replaces old (nil when the core holds none)
// with next: the entries that changed or went away are deleted, those that
// changed or are new inserted.
func tableWrite(old, next *part) pstate.TableWrite {
	w := pstate.TableWrite{Record: pstate.Owner{
		Owner: next.owner, Kind: next.kind, Schema: next.schema, Generation: int64(next.gen),
		Digest: string(next.digest), PushedAtMS: next.pushedAt,
	}}
	nextEntries := entriesOf(next.table)
	if old == nil || next.kind == extapi.KindFleetCapacity {
		w.DeleteAll = true
		w.Insert = rowsOf(next.owner, next.table, nil)
		return w
	}
	oldEntries := entriesOf(old.table)
	changed := map[string]bool{}
	for e, v := range nextEntries {
		if ov, ok := oldEntries[e]; !ok || !reflect.DeepEqual(ov, v) {
			changed[e] = true
		}
	}
	for e := range oldEntries {
		if _, ok := nextEntries[e]; !ok || changed[e] {
			w.DeleteEntries = append(w.DeleteEntries, e)
		}
	}
	sort.Strings(w.DeleteEntries)
	w.Insert = rowsOf(next.owner, next.table, changed)
	return w
}

type paramRows struct {
	owner, kind string
	out         *[]pstate.NoticeParam
}

func (p paramRows) add(entry, slot string, n *extapi.Notice) {
	if n == nil {
		return
	}
	keys := make([]string, 0, len(n.Params))
	for k := range n.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		row := pstate.NoticeParam{Owner: p.owner, Kind: p.kind, Entry: entry, Slot: slot, Key: k}
		switch v := n.Params[k].(type) {
		case string:
			row.Str = &v
		case bool:
			row.Bool = &v
		case int64:
			row.Int = &v
		default:
			continue // normalize keeps only these
		}
		*p.out = append(*p.out, row)
	}
}

func atMS(t *extapi.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// rowsOf is t's rows; with only set, just the entries it names (a kind's
// header row always).
func rowsOf(owner string, t extapi.Table, only map[string]bool) pstate.Rows {
	var r pstate.Rows
	keep := func(e string) bool { return only == nil || only[e] }
	params := paramRows{owner: owner, kind: t.TableKind(), out: &r.Params}
	switch t := t.(type) {
	case *extapi.AvailabilityV1:
		r.Availability = []pstate.AvailabilityTable{{Owner: owner, DownGraceS: t.DownGraceS}}
		for _, row := range t.Rows {
			if !keep(row.Feature) {
				continue
			}
			r.AvailabilityRows = append(r.AvailabilityRows, pstate.AvailabilityRow{Owner: owner, Feature: row.Feature, DownAccess: row.DownAccess})
			for i, s := range row.Steps {
				step := pstate.AvailabilityStep{Owner: owner, Feature: row.Feature, Step: i, AtMS: atMS(s.At), Access: s.Access, Link: s.Link}
				if s.Notice != nil {
					step.NoticeCode = s.Notice.Code
				}
				r.AvailabilitySteps = append(r.AvailabilitySteps, step)
				params.add(row.Feature, pstate.StepSlot(i), s.Notice)
				for _, l := range s.Limits {
					limit := pstate.AvailabilityLimit{Owner: owner, Feature: row.Feature, Step: i, Dim: l.Dim, Unlimited: l.Unlimited}
					if l.Max != nil {
						limit.Max = *l.Max
					}
					r.AvailabilityLimits = append(r.AvailabilityLimits, limit)
				}
			}
		}
	case *extapi.FleetCapacityV1:
		r.FleetCapacity = []pstate.FleetCapacity{{Owner: owner, Select: t.Select, RefuseNodeCode: t.RefuseNode.Code, RefuseCardCode: t.RefuseCard.Code, HoldCode: t.Hold.Code}}
		params.add(wholeTable, pstate.SlotRefuseNode, &t.RefuseNode)
		params.add(wholeTable, pstate.SlotRefuseCard, &t.RefuseCard)
		params.add(wholeTable, pstate.SlotHold, &t.Hold)
		for i, s := range t.Steps {
			r.CapacitySteps = append(r.CapacitySteps, pstate.CapacityStep{Owner: owner, Step: i, AtMS: atMS(s.At), MaxNodesWithCards: copyInt(s.MaxNodesWithCards), MaxCards: copyInt(s.MaxCards)})
		}
		for _, id := range t.Keep {
			r.CapacityKeep = append(r.CapacityKeep, pstate.CapacityKeep{Owner: owner, NodeID: id})
		}
	case *extapi.NodeHoldsV1:
		ids := make([]string, 0, len(t.Nodes))
		for id := range t.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !keep(id) {
				continue
			}
			n := t.Nodes[id]
			v, _ := strconv.ParseInt(id, 10, 64)
			r.NodeHolds = append(r.NodeHolds, pstate.NodeHold{Owner: owner, NodeID: v, Code: n.Code})
			params.add(id, pstate.SlotNode, &n)
		}
	case *extapi.BannersV1:
		for _, b := range t.Banners {
			if !keep(b.ID) {
				continue
			}
			r.Banners = append(r.Banners, pstate.Banner{Owner: owner, BannerID: b.ID, Audience: b.Audience})
			for i, s := range b.Steps {
				step := pstate.BannerStep{Owner: owner, BannerID: b.ID, Step: i, AtMS: atMS(s.At), Hidden: s.Hidden, Severity: s.Severity, Link: s.Link}
				if s.Notice != nil {
					step.NoticeCode = s.Notice.Code
				}
				r.BannerSteps = append(r.BannerSteps, step)
				params.add(b.ID, pstate.StepSlot(i), s.Notice)
			}
		}
	case *extapi.AttestationsV1:
		for _, a := range t.Attestations {
			if keep(a.Name) {
				r.Attestations = append(r.Attestations, pstate.Attestation{Owner: owner, Name: a.Name, Feature: a.Feature, Format: a.Format, Blob: a.Blob})
			}
		}
	}
	return r
}

// noticeKey names one stored notice.
type noticeKey struct{ owner, kind, entry, slot string }

// tablesOf rebuilds every owner's tables from a snapshot's rows, in
// normalized form. Rows without an owner record are left out.
func tablesOf(snap *pstate.Snapshot) (map[ownerKind]extapi.Table, error) {
	params := map[noticeKey]extapi.Params{}
	for _, p := range snap.Rows.Params {
		k := noticeKey{p.Owner, p.Kind, p.Entry, p.Slot}
		if params[k] == nil {
			params[k] = extapi.Params{}
		}
		switch {
		case p.Str != nil:
			params[k][p.Key] = *p.Str
		case p.Int != nil:
			params[k][p.Key] = *p.Int
		case p.Bool != nil:
			params[k][p.Key] = *p.Bool
		default:
			return nil, fmt.Errorf("pushed state: parameter %s of %s %s %s has no value", p.Key, p.Owner, p.Kind, p.Entry)
		}
	}
	notice := func(owner, kind, entry, slot, code string) *extapi.Notice {
		if code == "" {
			return nil
		}
		return &extapi.Notice{Code: code, Params: params[noticeKey{owner, kind, entry, slot}]}
	}
	// noticeValue is a required notice; a missing code leaves it empty, which
	// the table's validation then refuses.
	noticeValue := func(owner, kind, entry, slot, code string) extapi.Notice {
		if n := notice(owner, kind, entry, slot, code); n != nil {
			return *n
		}
		return extapi.Notice{}
	}
	out := map[ownerKind]extapi.Table{}
	for _, o := range snap.Owners {
		k := ownerKind{o.Owner, o.Kind}
		switch o.Kind {
		case extapi.KindAvailability:
			out[k] = &extapi.AvailabilityV1{}
		case extapi.KindFleetCapacity:
			out[k] = &extapi.FleetCapacityV1{}
		case extapi.KindNodeHolds:
			out[k] = &extapi.NodeHoldsV1{}
		case extapi.KindBanners:
			out[k] = &extapi.BannersV1{}
		case extapi.KindAttestations:
			out[k] = &extapi.AttestationsV1{}
		default:
			return nil, fmt.Errorf("pushed state: owner %s holds a %q table this build does not know", o.Owner, o.Kind)
		}
	}
	avail := func(owner string) *extapi.AvailabilityV1 {
		t, _ := out[ownerKind{owner, extapi.KindAvailability}].(*extapi.AvailabilityV1)
		return t
	}
	r := &snap.Rows
	for _, h := range r.Availability {
		if t := avail(h.Owner); t != nil {
			t.DownGraceS = h.DownGraceS
		}
	}
	rowAt := map[[2]string]int{}
	for _, row := range r.AvailabilityRows {
		if t := avail(row.Owner); t != nil {
			rowAt[[2]string{row.Owner, row.Feature}] = len(t.Rows)
			t.Rows = append(t.Rows, extapi.AvailabilityRow{Feature: row.Feature, DownAccess: row.DownAccess})
		}
	}
	for _, s := range r.AvailabilitySteps {
		t, i, ok := avail(s.Owner), 0, false
		if t != nil {
			i, ok = rowAt[[2]string{s.Owner, s.Feature}]
		}
		if !ok {
			continue
		}
		t.Rows[i].Steps = append(t.Rows[i].Steps, extapi.AvailabilityStep{
			At: timeOf(s.AtMS), Access: s.Access, Link: s.Link,
			Notice: notice(s.Owner, extapi.KindAvailability, s.Feature, pstate.StepSlot(s.Step), s.NoticeCode),
		})
	}
	for _, l := range r.AvailabilityLimits {
		t, i, ok := avail(l.Owner), 0, false
		if t != nil {
			i, ok = rowAt[[2]string{l.Owner, l.Feature}]
		}
		if !ok || l.Step >= len(t.Rows[i].Steps) {
			continue
		}
		limit := extapi.Limit{Dim: l.Dim, Unlimited: l.Unlimited}
		if !l.Unlimited {
			v := l.Max
			limit.Max = &v
		}
		st := &t.Rows[i].Steps[l.Step]
		st.Limits = append(st.Limits, limit)
	}
	for _, f := range r.FleetCapacity {
		if t, ok := out[ownerKind{f.Owner, extapi.KindFleetCapacity}].(*extapi.FleetCapacityV1); ok {
			t.Select = f.Select
			t.RefuseNode = noticeValue(f.Owner, extapi.KindFleetCapacity, wholeTable, pstate.SlotRefuseNode, f.RefuseNodeCode)
			t.RefuseCard = noticeValue(f.Owner, extapi.KindFleetCapacity, wholeTable, pstate.SlotRefuseCard, f.RefuseCardCode)
			t.Hold = noticeValue(f.Owner, extapi.KindFleetCapacity, wholeTable, pstate.SlotHold, f.HoldCode)
		}
	}
	for _, s := range r.CapacitySteps {
		if t, ok := out[ownerKind{s.Owner, extapi.KindFleetCapacity}].(*extapi.FleetCapacityV1); ok {
			t.Steps = append(t.Steps, extapi.CapacityStep{At: timeOf(s.AtMS), MaxNodesWithCards: copyInt(s.MaxNodesWithCards), MaxCards: copyInt(s.MaxCards)})
		}
	}
	for _, k := range r.CapacityKeep {
		if t, ok := out[ownerKind{k.Owner, extapi.KindFleetCapacity}].(*extapi.FleetCapacityV1); ok {
			t.Keep = append(t.Keep, k.NodeID)
		}
	}
	for _, h := range r.NodeHolds {
		if t, ok := out[ownerKind{h.Owner, extapi.KindNodeHolds}].(*extapi.NodeHoldsV1); ok {
			if t.Nodes == nil {
				t.Nodes = map[string]extapi.Notice{}
			}
			id := strconv.FormatInt(h.NodeID, 10)
			t.Nodes[id] = noticeValue(h.Owner, extapi.KindNodeHolds, id, pstate.SlotNode, h.Code)
		}
	}
	bannerAt := map[[2]string]int{}
	for _, b := range r.Banners {
		if t, ok := out[ownerKind{b.Owner, extapi.KindBanners}].(*extapi.BannersV1); ok {
			bannerAt[[2]string{b.Owner, b.BannerID}] = len(t.Banners)
			t.Banners = append(t.Banners, extapi.Banner{ID: b.BannerID, Audience: b.Audience})
		}
	}
	for _, s := range r.BannerSteps {
		t, ok := out[ownerKind{s.Owner, extapi.KindBanners}].(*extapi.BannersV1)
		i, found := bannerAt[[2]string{s.Owner, s.BannerID}]
		if !ok || !found {
			continue
		}
		t.Banners[i].Steps = append(t.Banners[i].Steps, extapi.BannerStep{
			At: timeOf(s.AtMS), Hidden: s.Hidden, Severity: s.Severity, Link: s.Link,
			Notice: notice(s.Owner, extapi.KindBanners, s.BannerID, pstate.StepSlot(s.Step), s.NoticeCode),
		})
	}
	for _, a := range r.Attestations {
		if t, ok := out[ownerKind{a.Owner, extapi.KindAttestations}].(*extapi.AttestationsV1); ok {
			t.Attestations = append(t.Attestations, extapi.Attestation{Name: a.Name, Feature: a.Feature, Format: a.Format, Blob: a.Blob})
		}
	}
	for k, t := range out {
		out[k] = normalize(t)
	}
	return out, nil
}

func timeOf(ms int64) *extapi.Time {
	if ms == 0 {
		return nil
	}
	t := extapi.NewTime(time.UnixMilli(ms))
	return &t
}
