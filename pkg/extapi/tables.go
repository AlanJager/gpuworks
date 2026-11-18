package extapi

import (
	"strconv"
)

// Row limits per kind at schema 1. A push over a limit fails with
// ErrorStateTooLarge.
const (
	MaxAvailabilityRows = 4096
	MaxNodeHolds        = 10000
	MaxBanners          = 16
	MaxAttestations     = 1024
	MaxAttestationBlob  = 64 << 10
	MaxSteps            = 64
	MaxLimits           = 16
)

// Down grace bounds, in seconds: how long a down owner's grants stay as
// pushed before they clamp to the row's down access.
const (
	MinDownGraceS = 60
	MaxDownGraceS = 86400
)

// Feature access levels on the wire.
const (
	AccessFull     = "full"
	AccessReadOnly = "read_only"
	AccessLocked   = "locked"
	AccessHidden   = "hidden"
)

// Notice is a product code and its parameters (ADR 0067), as the server
// shows it: a refusal, a banner, a hold reason. The code must be one the
// pushing package lists in its manifest.
type Notice struct {
	Code   string `json:"code"`
	Params Params `json:"params,omitempty"`
}

func (n Notice) validate(path string) *fieldError {
	return first(
		matches(joinPath(path, "code"), n.Code, codePattern),
		n.Params.validate(joinPath(path, "params")),
	)
}

// Limit is one quantity a feature is available up to. Exactly one of Max
// and Unlimited is set.
type Limit struct {
	Dim       string `json:"dim"`
	Max       *int64 `json:"max,omitempty"`
	Unlimited bool   `json:"unlimited,omitempty"`
}

// AvailabilityV1 is table.availability@1: the answer for each feature key
// the owner is the authority for (or declared with gate:self), as a
// timeline the server follows on its own clock.
type AvailabilityV1 struct {
	// DownGraceS is how long, in seconds, the rows stay as pushed after
	// the owner goes down; 0 takes the server's default. The server bounds
	// it to [MinDownGraceS, MaxDownGraceS] and a site may only lower it.
	DownGraceS int               `json:"down_grace_s,omitempty"`
	Rows       []AvailabilityRow `json:"rows,omitempty"`
}

// AvailabilityRow is one feature key's timeline.
type AvailabilityRow struct {
	Feature string `json:"feature"`
	// DownAccess is what the key clamps to once the owner has been down
	// longer than the grace: read_only or locked, never wider than the
	// current step.
	DownAccess string             `json:"down_access"`
	Steps      []AvailabilityStep `json:"steps"`
	Display    Display            `json:"display,omitempty"`
}

// AvailabilityStep is the answer from At on. Only the first step may omit
// At (it holds from the push); later steps are strictly increasing.
type AvailabilityStep struct {
	At     *Time   `json:"at,omitempty"`
	Access string  `json:"access"`
	Notice *Notice `json:"notice,omitempty"`
	Link   string  `json:"link,omitempty"`
	// Limits are sorted by Dim.
	Limits []Limit `json:"limits,omitempty"`
}

func (*AvailabilityV1) TableKind() string { return KindAvailability }
func (*AvailabilityV1) TableSchema() int  { return 1 }

// Validate checks the body's rules.
func (t *AvailabilityV1) Validate() error {
	if len(t.Rows) > MaxAvailabilityRows {
		return tooLarge(KindAvailability, "rows", MaxAvailabilityRows)
	}
	if t.DownGraceS != 0 {
		if f := inRange("down_grace_s", int64(t.DownGraceS), MinDownGraceS, MaxDownGraceS); f != nil {
			return stateRejected(KindAvailability, f)
		}
	}
	seen := map[string]bool{}
	for i, r := range t.Rows {
		at := joinPath("rows", strconv.Itoa(i))
		if len(r.Steps) > MaxSteps {
			return tooLarge(KindAvailability, joinPath(at, "steps"), MaxSteps)
		}
		f := first(
			matches(joinPath(at, "feature"), r.Feature, featureKeyPattern),
			oneOf(joinPath(at, "down_access"), r.DownAccess, AccessReadOnly, AccessLocked),
			r.Display.validate(joinPath(at, "display")),
		)
		if f == nil && seen[r.Feature] {
			f = &fieldError{field: joinPath(at, "feature"), rule: RuleDuplicate}
		}
		seen[r.Feature] = true
		if f == nil {
			f = validateTimeline(joinPath(at, "steps"), len(r.Steps), func(i int) *Time { return r.Steps[i].At })
		}
		for j := 0; f == nil && j < len(r.Steps); j++ {
			f = r.Steps[j].validate(joinPath(joinPath(at, "steps"), strconv.Itoa(j)))
		}
		if f != nil {
			f.row = r.Feature
			return stateRejected(KindAvailability, f)
		}
	}
	return nil
}

func (s AvailabilityStep) validate(path string) *fieldError {
	if f := first(
		oneOf(joinPath(path, "access"), s.Access, AccessFull, AccessReadOnly, AccessLocked, AccessHidden),
		optionalMatches(joinPath(path, "link"), s.Link, linkPattern),
	); f != nil {
		return f
	}
	if s.Notice != nil {
		if f := s.Notice.validate(joinPath(path, "notice")); f != nil {
			return f
		}
	}
	if len(s.Limits) > MaxLimits {
		return &fieldError{field: joinPath(path, "limits"), rule: RuleRange}
	}
	for i, l := range s.Limits {
		at := joinPath(joinPath(path, "limits"), strconv.Itoa(i))
		if f := matches(joinPath(at, "dim"), l.Dim, wordPattern); f != nil {
			return f
		}
		if i > 0 && s.Limits[i-1].Dim >= l.Dim {
			rule := RuleOrder
			if s.Limits[i-1].Dim == l.Dim {
				rule = RuleDuplicate
			}
			return &fieldError{field: joinPath(at, "dim"), rule: rule}
		}
		if (l.Max != nil) == l.Unlimited {
			return &fieldError{field: joinPath(at, "max"), rule: RuleExclusive}
		}
		if l.Max != nil {
			if f := inRange(joinPath(at, "max"), *l.Max, 0, maxSafeInteger); f != nil {
				return f
			}
		}
	}
	return nil
}

// validateTimeline checks a list of n steps: at least one, at most
// MaxSteps, only the first without a time, times strictly increasing.
func validateTimeline(path string, n int, at func(int) *Time) *fieldError {
	if n == 0 {
		return &fieldError{field: path, rule: RuleRequired}
	}
	var prev *Time
	for i := range n {
		t := at(i)
		field := joinPath(joinPath(path, strconv.Itoa(i)), "at")
		if t == nil {
			if i > 0 {
				return &fieldError{field: field, rule: RuleRequired}
			}
			continue
		}
		if t.IsZero() {
			return &fieldError{field: field, rule: RuleRequired}
		}
		if prev != nil && !t.After(prev.Time) {
			return &fieldError{field: field, rule: RuleOrder}
		}
		prev = t
	}
	return nil
}

// Capacity selection rules.
const (
	// SelectEnrollmentOrder keeps nodes in the order they joined, the
	// table's kept nodes first, until a cap is full and holds the rest.
	SelectEnrollmentOrder = "enrollment_order"
)

// FleetCapacityV1 is table.fleet_capacity@1: caps on the fleet the server
// enforces at node enrollment and by holding over-cap nodes from
// scheduling. The owner pushes caps and, optionally, the nodes to keep
// first; the server selects the nodes.
type FleetCapacityV1 struct {
	Select string `json:"select"`
	// RefuseNode and RefuseCard refuse an enrollment over a cap; Hold is
	// the reason on a node held over a cap.
	RefuseNode Notice         `json:"refuse_node"`
	RefuseCard Notice         `json:"refuse_card"`
	Hold       Notice         `json:"hold"`
	Steps      []CapacityStep `json:"steps"`
	// Keep lists server node ids, strictly ascending, that the selection
	// considers before every other node; at most MaxNodeHolds. An id that
	// names no node, or a node without cards, matches nothing. Keeping more
	// than the caps allow is not an error: the overflow is held like any
	// other node.
	Keep []int64 `json:"keep,omitempty"`
}

// CapacityStep is the caps from At on; an absent cap is unlimited.
type CapacityStep struct {
	At                *Time  `json:"at,omitempty"`
	MaxNodesWithCards *int64 `json:"max_nodes_with_cards,omitempty"`
	MaxCards          *int64 `json:"max_cards,omitempty"`
}

func (*FleetCapacityV1) TableKind() string { return KindFleetCapacity }
func (*FleetCapacityV1) TableSchema() int  { return 1 }

// Validate checks the body's rules.
func (t *FleetCapacityV1) Validate() error {
	if len(t.Steps) > MaxSteps {
		return tooLarge(KindFleetCapacity, "steps", MaxSteps)
	}
	if len(t.Keep) > MaxNodeHolds {
		return tooLarge(KindFleetCapacity, "keep", MaxNodeHolds)
	}
	f := first(
		oneOf("select", t.Select, SelectEnrollmentOrder),
		t.RefuseNode.validate("refuse_node"),
		t.RefuseCard.validate("refuse_card"),
		t.Hold.validate("hold"),
		validateTimeline("steps", len(t.Steps), func(i int) *Time { return t.Steps[i].At }),
	)
	for i := 0; f == nil && i < len(t.Steps); i++ {
		at := joinPath("steps", strconv.Itoa(i))
		s := t.Steps[i]
		if s.MaxNodesWithCards != nil {
			f = inRange(joinPath(at, "max_nodes_with_cards"), *s.MaxNodesWithCards, 0, maxSafeInteger)
		}
		if f == nil && s.MaxCards != nil {
			f = inRange(joinPath(at, "max_cards"), *s.MaxCards, 0, maxSafeInteger)
		}
	}
	for i := 0; f == nil && i < len(t.Keep); i++ {
		at := joinPath("keep", strconv.Itoa(i))
		f = inRange(at, t.Keep[i], 1, maxSafeInteger)
		if f == nil && i > 0 && t.Keep[i-1] >= t.Keep[i] {
			rule := RuleOrder
			if t.Keep[i-1] == t.Keep[i] {
				rule = RuleDuplicate
			}
			f = &fieldError{field: at, rule: rule}
		}
	}
	if f != nil {
		return stateRejected(KindFleetCapacity, f)
	}
	return nil
}
