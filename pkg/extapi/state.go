package extapi

import (
	"encoding/json"
	"strconv"
)

const (
	objectStatePush = "state_push"
	objectStateAck  = "state_ack"
)

// MaxPushBytes bounds one push envelope, encoded.
const MaxPushBytes = 16 << 20

// Class is how the server combines one kind's rows from several owners.
type Class uint8

const (
	// Restrict rows combine by intersection, from any approved owner, and
	// never lapse while the owner is down.
	Restrict Class = iota + 1
	// Grant rows come only from the namespace authority (or the declaring
	// package for gate:self keys) and clamp after the owner's down grace.
	Grant
	// DisplayClass rows are shown, never enforced.
	DisplayClass
	// Opaque rows are stored and relayed, never interpreted.
	Opaque
)

var kindClasses = map[string]Class{
	KindAvailability:  Grant,
	KindFleetCapacity: Restrict,
	KindNodeHolds:     Restrict,
	KindBanners:       DisplayClass,
	KindAttestations:  Opaque,
}

// ClassOf returns the class of a kind this package defines, or 0.
func ClassOf(kind string) Class { return kindClasses[kind] }

// StatePush is the body of PUT /host/v1/state/{kind}: one owner's whole
// table of one kind. The owner is the package bound to the connection; no
// field names it.
type StatePush struct {
	// Kind is the table kind, the same as the path segment.
	Kind string `json:"kind"`
	// Schema is the version of Kind's rows in Body, one the plugin
	// negotiated as table.<kind>@<schema>.
	Schema int `json:"schema"`
	// Generation increases strictly per owner and kind.
	Generation uint64 `json:"generation"`
	// Base is the generation a patch applies to; only kinds that define a
	// patch body set it, and no kind of schema 1 does.
	Base uint64 `json:"base,omitempty"`
	// Body is the table, decoded strictly at Kind and Schema.
	Body json.RawMessage `json:"body"`
}

// Table is a decoded push body.
type Table interface {
	// TableKind and TableSchema name the body's kind and schema.
	TableKind() string
	TableSchema() int
	// Validate checks every rule of the kind that needs only the body.
	Validate() error
}

// DecodeStatePush decodes and validates a push envelope; DecodeBody then
// decodes its table.
func DecodeStatePush(data []byte) (*StatePush, error) {
	if len(data) > MaxPushBytes {
		return nil, tooLarge("", "body", MaxPushBytes)
	}
	var p StatePush
	if err := decodeStrict(objectStatePush, data, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the envelope's rules.
func (p *StatePush) Validate() error {
	f := first(
		matches("kind", p.Kind, wordPattern),
		inRange("schema", int64(p.Schema), 1, maxSafeInteger),
		inRange("generation", int64(min(p.Generation, maxSafeInteger+1)), 1, maxSafeInteger),
	)
	if f == nil && p.Base != 0 {
		f = &fieldError{field: "base", rule: RuleRelation}
	}
	if f == nil && (len(p.Body) == 0 || p.Body[0] != '{') {
		f = &fieldError{field: "body", rule: RuleType}
	}
	if f != nil {
		return wireInvalid(objectStatePush, f)
	}
	return nil
}

// DecodeBody decodes the push's table at its kind and schema. A well-formed
// kind or a schema of at least 1 that this package does not define fails
// with ErrorSchemaTooNew: a newer server defines it.
func (p *StatePush) DecodeBody() (Table, error) {
	return DecodeTable(p.Kind, p.Schema, p.Body)
}

// DecodeTable decodes and validates a table body of kind at schema.
func DecodeTable(kind string, schema int, body []byte) (Table, error) {
	var t Table
	switch {
	case kind == KindAvailability && schema == 1:
		t = &AvailabilityV1{}
	case kind == KindFleetCapacity && schema == 1:
		t = &FleetCapacityV1{}
	case kind == KindNodeHolds && schema == 1:
		t = &NodeHoldsV1{}
	case kind == KindBanners && schema == 1:
		t = &BannersV1{}
	case kind == KindAttestations && schema == 1:
		t = &AttestationsV1{}
	case !wordPattern.MatchString(kind):
		return nil, wireInvalid(objectStatePush, &fieldError{field: "kind", rule: RuleGrammar})
	case schema < 1:
		return nil, wireInvalid(objectStatePush, &fieldError{field: "schema", rule: RuleRange})
	default:
		known := 0
		if s := tableSchemas[kind]; len(s) > 0 {
			known = s[len(s)-1]
		}
		return nil, newError(ErrorSchemaTooNew,
			map[string]any{"object": TableCapability(kind), "version": schema, "known": known},
			"table %s@%d is not one this build reads (up to %d)", kind, schema, known)
	}
	if err := decodeStrict(tableObject(kind, schema), body, t); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

func tableObject(kind string, schema int) string {
	return TableCapability(kind) + "@" + strconv.Itoa(schema)
}

func tooLarge(kind, field string, limit int) *Error {
	return newError(ErrorStateTooLarge, map[string]any{"kind": kind, "field": field, "max": limit},
		"pushed %s: %s exceeds %d", kind, field, limit)
}

// StateAck answers a push: either the server applied it (AppliedAt and
// Digest set) or a newer push of the same owner and kind replaced it before
// it was committed (SupersededBy set).
type StateAck struct {
	Kind         string `json:"kind"`
	Generation   uint64 `json:"generation"`
	Digest       Digest `json:"digest,omitempty"`
	AppliedAt    *Time  `json:"applied_at,omitempty"`
	SupersededBy uint64 `json:"superseded_by,omitempty"`
}

// DecodeStateAck decodes and validates an acknowledgement.
func DecodeStateAck(data []byte) (*StateAck, error) {
	var a StateAck
	if err := decodeStrict(objectStateAck, data, &a); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// Validate checks the acknowledgement's rules.
func (a *StateAck) Validate() error {
	f := first(
		matches("kind", a.Kind, wordPattern),
		inRange("generation", int64(min(a.Generation, maxSafeInteger+1)), 1, maxSafeInteger),
		inRange("superseded_by", int64(min(a.SupersededBy, maxSafeInteger+1)), 0, maxSafeInteger),
	)
	applied := a.AppliedAt != nil
	if f == nil && applied == (a.SupersededBy != 0) {
		f = &fieldError{field: "applied_at", rule: RuleExclusive}
	}
	if f == nil && applied {
		f = matches("digest", string(a.Digest), digestPattern)
	}
	if f == nil && !applied && a.Digest != "" {
		f = &fieldError{field: "digest", rule: RuleRelation}
	}
	if f == nil && !applied && a.SupersededBy <= a.Generation {
		f = &fieldError{field: "superseded_by", rule: RuleRelation}
	}
	if f != nil {
		return wireInvalid(objectStateAck, f)
	}
	return nil
}
