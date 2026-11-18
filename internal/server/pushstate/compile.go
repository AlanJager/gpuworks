package pushstate

import (
	"encoding/json"
	"math"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// noTime is the instant of a first step without a time: it holds from the
// push on, before any other instant.
const noTime = math.MinInt64

// part is one owner's committed (or pending) table of one kind: the table in
// its normalized form, which is what is persisted and compared, and the
// compiled form the snapshot is combined from.
type part struct {
	owner    string
	kind     string
	schema   int
	gen      uint64
	digest   extapi.Digest
	pushedAt int64
	table    extapi.Table

	avail    *availPart
	capacity *capacityPart
	holds    map[uint]Hold
	banners  []bannerPart
}

type availPart struct {
	downGraceS int
	rows       []availRow
}

type availRow struct {
	key        featuregate.Key
	downAccess featuregate.Access
	steps      []availStep
}

type availStep struct {
	at int64
	av featuregate.Availability
}

type capacityPart struct {
	steps []capacityStep
	// keep are the node ids the table keeps first, ascending.
	keep       []uint
	refuseNode *messages.ProductError
	refuseCard *messages.ProductError
	hold       Hold
}

type capacityStep struct {
	at   int64
	caps fleetcap.Caps
}

type bannerPart struct {
	id    string
	admin bool
	steps []bannerStep
}

type bannerStep struct {
	at     int64
	banner Banner
}

// normalize returns t in the one form the core keeps: display-only data
// dropped (the core never shows it), parameters with integers as int64 and
// empty parameter maps absent, and times rebuilt from their milliseconds, so
// a table read back from the database equals the one that was pushed.
func normalize(t extapi.Table) extapi.Table {
	switch t := t.(type) {
	case *extapi.AvailabilityV1:
		out := &extapi.AvailabilityV1{DownGraceS: t.DownGraceS}
		for _, r := range t.Rows {
			row := extapi.AvailabilityRow{Feature: r.Feature, DownAccess: r.DownAccess}
			for _, s := range r.Steps {
				step := extapi.AvailabilityStep{At: normTime(s.At), Access: s.Access, Notice: normNotice(s.Notice), Link: s.Link}
				for _, l := range s.Limits {
					limit := extapi.Limit{Dim: l.Dim, Unlimited: l.Unlimited}
					if l.Max != nil {
						v := *l.Max
						limit.Max = &v
					}
					step.Limits = append(step.Limits, limit)
				}
				row.Steps = append(row.Steps, step)
			}
			out.Rows = append(out.Rows, row)
		}
		return out
	case *extapi.FleetCapacityV1:
		out := &extapi.FleetCapacityV1{Select: t.Select, RefuseNode: *normNotice(&t.RefuseNode), RefuseCard: *normNotice(&t.RefuseCard), Hold: *normNotice(&t.Hold)}
		for _, s := range t.Steps {
			out.Steps = append(out.Steps, extapi.CapacityStep{At: normTime(s.At), MaxNodesWithCards: copyInt(s.MaxNodesWithCards), MaxCards: copyInt(s.MaxCards)})
		}
		if len(t.Keep) > 0 {
			out.Keep = append([]int64(nil), t.Keep...)
		}
		return out
	case *extapi.NodeHoldsV1:
		out := &extapi.NodeHoldsV1{}
		if len(t.Nodes) > 0 {
			out.Nodes = make(map[string]extapi.Notice, len(t.Nodes))
			for id, n := range t.Nodes {
				out.Nodes[id] = *normNotice(&n)
			}
		}
		return out
	case *extapi.BannersV1:
		out := &extapi.BannersV1{}
		for _, b := range t.Banners {
			banner := extapi.Banner{ID: b.ID, Audience: b.Audience}
			for _, s := range b.Steps {
				banner.Steps = append(banner.Steps, extapi.BannerStep{At: normTime(s.At), Hidden: s.Hidden, Severity: s.Severity, Notice: normNotice(s.Notice), Link: s.Link})
			}
			out.Banners = append(out.Banners, banner)
		}
		return out
	case *extapi.AttestationsV1:
		out := &extapi.AttestationsV1{}
		for _, a := range t.Attestations {
			out.Attestations = append(out.Attestations, extapi.Attestation{Name: a.Name, Feature: a.Feature, Format: a.Format, Blob: append([]byte(nil), a.Blob...)})
		}
		return out
	}
	return t
}

func normTime(t *extapi.Time) *extapi.Time {
	if t == nil {
		return nil
	}
	n := extapi.NewTime(time.UnixMilli(t.UnixMilli()))
	return &n
}

func copyInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func normNotice(n *extapi.Notice) *extapi.Notice {
	if n == nil {
		return nil
	}
	return &extapi.Notice{Code: n.Code, Params: normParams(n.Params)}
}

// normParams turns numbers into int64 (the wire's json.Number, the
// database's int64) and an empty map into nil.
func normParams(p extapi.Params) extapi.Params {
	if len(p) == 0 {
		return nil
	}
	out := make(extapi.Params, len(p))
	for k, v := range p {
		switch v := v.(type) {
		case json.Number:
			n, _ := v.Int64()
			out[k] = n
		case int:
			out[k] = int64(v)
		default:
			out[k] = v
		}
	}
	return out
}

func timeMS(t *extapi.Time) int64 {
	if t == nil {
		return noTime
	}
	return t.UnixMilli()
}

// accessOf is the gate's access for a wire access level.
func accessOf(s string) featuregate.Access {
	switch s {
	case extapi.AccessFull:
		return featuregate.Full
	case extapi.AccessReadOnly:
		return featuregate.ReadOnly
	case extapi.AccessLocked:
		return featuregate.Locked
	}
	return featuregate.Hidden
}

// productError is a pushed notice as the console is shown it. The code's
// text is the package's; the English fallback only names the code.
func productError(n *extapi.Notice, subject string) *messages.ProductError {
	if n == nil {
		return nil
	}
	var params map[string]any
	if len(n.Params) > 0 {
		params = map[string]any(n.Params)
	}
	return messages.NewProductError(n.Code, subject+": "+n.Code, params)
}

// compile fills p's compiled form from its normalized table.
func compile(p *part) {
	switch t := p.table.(type) {
	case *extapi.AvailabilityV1:
		p.avail = compileAvailability(t)
	case *extapi.FleetCapacityV1:
		p.capacity = compileCapacity(p.owner, t)
	case *extapi.NodeHoldsV1:
		p.holds = make(map[uint]Hold, len(t.Nodes))
		for id, n := range t.Nodes {
			v, err := strconv.ParseUint(id, 10, 0)
			if err != nil {
				continue
			}
			p.holds[uint(v)] = holdOf(&n, "the node is held out of scheduling by plugin "+p.owner)
		}
	case *extapi.BannersV1:
		p.banners = compileBanners(p.owner, t)
	}
}

func compileAvailability(t *extapi.AvailabilityV1) *availPart {
	out := &availPart{downGraceS: t.DownGraceS, rows: make([]availRow, 0, len(t.Rows))}
	for _, r := range t.Rows {
		row := availRow{key: featuregate.Key(r.Feature), downAccess: accessOf(r.DownAccess), steps: make([]availStep, 0, len(r.Steps))}
		for _, s := range r.Steps {
			av := featuregate.Availability{Access: accessOf(s.Access), Notice: productError(s.Notice, "feature "+r.Feature), Link: s.Link}
			for _, l := range s.Limits {
				limit := featuregate.Limit{Dim: l.Dim, Unlimited: l.Unlimited}
				if l.Max != nil {
					limit.Max = *l.Max
				}
				av.Limits = append(av.Limits, limit)
			}
			row.steps = append(row.steps, availStep{at: timeMS(s.At), av: av})
		}
		out.rows = append(out.rows, row)
	}
	return out
}

func compileCapacity(owner string, t *extapi.FleetCapacityV1) *capacityPart {
	out := &capacityPart{
		refuseNode: productError(&t.RefuseNode, "a new node cannot join: the node capacity set by plugin "+owner+" is full"),
		refuseCard: productError(&t.RefuseCard, "a new node cannot join: the card capacity set by plugin "+owner+" is full"),
		hold:       holdOf(&t.Hold, "the node exceeds the fleet capacity set by plugin "+owner+" and takes no new work; what runs there keeps running"),
	}
	for _, s := range t.Steps {
		out.steps = append(out.steps, capacityStep{at: timeMS(s.At), caps: fleetcap.Caps{NodesWithCards: limitOf(s.MaxNodesWithCards), Cards: limitOf(s.MaxCards)}})
	}
	for _, id := range t.Keep {
		out.keep = append(out.keep, uint(id))
	}
	return out
}

func limitOf(v *int64) fleetcap.Limit {
	if v == nil {
		return fleetcap.Limit{}
	}
	return fleetcap.Limit{Limited: true, Max: *v}
}

func holdOf(n *extapi.Notice, fallback string) Hold {
	h := Hold{Code: n.Code, Fallback: fallback}
	if len(n.Params) > 0 {
		h.Params = map[string]any(n.Params)
	}
	return h
}

func compileBanners(owner string, t *extapi.BannersV1) []bannerPart {
	out := make([]bannerPart, 0, len(t.Banners))
	for _, b := range t.Banners {
		bp := bannerPart{id: owner + "." + b.ID, admin: b.Audience == extapi.AudienceAdmin}
		for _, s := range b.Steps {
			step := bannerStep{at: timeMS(s.At)}
			if !s.Hidden {
				step.banner = Banner{ID: bp.id, Severity: s.Severity, Link: s.Link, Notice: productError(s.Notice, "plugin "+owner)}
			}
			bp.steps = append(bp.steps, step)
		}
		out = append(out, bp)
	}
	return out
}

// stepAt is the index of the step in force at now among n steps whose
// instants at(i) ascend, or -1 before the first.
func stepAt(n int, at func(int) int64, now int64) int {
	i := -1
	for j := 0; j < n; j++ {
		if at(j) > now {
			break
		}
		i = j
	}
	return i
}

// nextAfter is the earliest instant among n ascending instants after now,
// or math.MaxInt64.
func nextAfter(n int, at func(int) int64, now int64) int64 {
	for j := 0; j < n; j++ {
		if a := at(j); a > now {
			return a
		}
	}
	return math.MaxInt64
}
