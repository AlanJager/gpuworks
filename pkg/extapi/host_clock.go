package extapi

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

// Object names of the host API's clock, readiness and own-state calls.
const (
	objectClockCorrection = "clock_correction"
	objectClockCorrected  = "clock_corrected"
	objectHostMeta        = "host_meta"
	objectPushedState     = "pushed_state"
)

// The host API's clock, readiness and own-state calls, frozen with protocol
// major 1.
const (
	// PathHostMeta answers GET with a HostMeta. It needs CapHostClock.
	PathHostMeta = "/host/v1/meta"
	// PathHostClockCorrect answers POST of a ClockCorrection with a
	// ClockCorrected. It needs CapHostClock and RoleClockAuthority.
	PathHostClockCorrect = "/host/v1/clock/correct"
	// PathHostReady answers POST (no body) with 204: the plugin has
	// finished starting, so a later down period of this process gets no
	// boot extension before its grace. It is part of the base protocol;
	// a second call, or one from an on-demand plugin, changes nothing.
	PathHostReady = "/host/v1/state/ready"
	// PathHostState+kind answers GET with the PushedState the server holds
	// for the caller's own table of kind. It needs CapHostStateRead.
)

// Capabilities and roles of these calls.
const (
	// CapHostClock offers GET meta and POST clock/correct.
	CapHostClock = "host.clock"
	// CapHostStateRead offers GET of the caller's own pushed tables.
	CapHostStateRead = "host.state_read"
	// RoleClockAuthority is the privileged role a package needs to
	// correct the server's extclock; only a certificate or the host
	// operator grants it.
	RoleClockAuthority = "clock_authority"
)

// Bounds of these calls.
const (
	// MaxClockReason bounds a correction's reason, in bytes.
	MaxClockReason = 256
	// MaxHostCallBytes bounds the encoded body of a clock correction or a
	// key request; the server refuses a larger one as too large.
	MaxHostCallBytes = 4 << 10
	// MaxPushedStateBytes bounds an encoded own-state answer: a whole
	// pushed table and its record.
	MaxPushedStateBytes = MaxPushBytes + 4<<10
)

// bootIDPattern is a server boot id: 32 lowercase hex characters.
var bootIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ClockCorrection is the body of POST /host/v1/clock/correct: move the
// server's extclock to To, which the plugin has verified (for example from
// a correction code its publisher signed). One correction moves extclock at
// most 366 days either way; steps are not consumed, so moving it back
// un-fires the steps after To.
type ClockCorrection struct {
	To Time `json:"to"`
	// Reason is recorded in the server's audit log; 1 to MaxClockReason
	// bytes of UTF-8.
	Reason string `json:"reason"`
}

// DecodeClockCorrection decodes and validates a correction request.
func DecodeClockCorrection(data []byte) (*ClockCorrection, error) {
	if len(data) > MaxHostCallBytes {
		return nil, malformed(objectClockCorrection, &fieldError{rule: RuleRange})
	}
	var c ClockCorrection
	if err := decodeStrict(objectClockCorrection, data, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the request's rules.
func (c *ClockCorrection) Validate() error {
	f := first(requiredTime("to", c.To), text("reason", c.Reason, MaxClockReason))
	if f == nil && !utf8.ValidString(c.Reason) {
		f = &fieldError{field: "reason", rule: RuleGrammar}
	}
	if f != nil {
		return wireInvalid(objectClockCorrection, f)
	}
	return nil
}

// ClockCorrected answers a correction: extclock before and after it.
type ClockCorrected struct {
	From Time `json:"from"`
	To   Time `json:"to"`
}

// DecodeClockCorrected decodes and validates a correction's answer.
func DecodeClockCorrected(data []byte) (*ClockCorrected, error) {
	var c ClockCorrected
	if err := decodeStrict(objectClockCorrected, data, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the answer's rules.
func (c *ClockCorrected) Validate() error {
	if f := first(requiredTime("from", c.From), requiredTime("to", c.To)); f != nil {
		return wireInvalid(objectClockCorrected, f)
	}
	return nil
}

// HostMeta is the body of GET /host/v1/meta: the server's version and boot,
// and its clocks.
type HostMeta struct {
	// Version is the server version, as in the handshake.
	Version string `json:"version"`
	// BootID changes every time the server starts.
	BootID string `json:"boot_id"`
	// Wall is the server's wall clock; ExtClock the monotone clock
	// timelines follow (never behind the wall clock or the high-water
	// mark).
	Wall     Time `json:"wall"`
	ExtClock Time `json:"extclock"`
	// HWM is the high-water mark the server last stored, absent before it
	// stored one.
	HWM *Time `json:"hwm,omitempty"`
}

// DecodeHostMeta decodes and validates a meta answer.
func DecodeHostMeta(data []byte) (*HostMeta, error) {
	var m HostMeta
	if err := decodeStrict(objectHostMeta, data, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the answer's rules.
func (m *HostMeta) Validate() error {
	f := first(
		matches("version", m.Version, coreVersionPattern),
		matches("boot_id", m.BootID, bootIDPattern),
		requiredTime("wall", m.Wall),
		requiredTime("extclock", m.ExtClock),
	)
	if f == nil && m.HWM != nil {
		f = requiredTime("hwm", *m.HWM)
	}
	if f != nil {
		return wireInvalid(objectHostMeta, f)
	}
	return nil
}

// PushedState answers GET /host/v1/state/{kind}: the caller's own table of
// kind as the server holds it, so a restarted plugin can reconcile. Only
// committed pushes are held. When the server holds nothing for the caller,
// only Kind is set.
type PushedState struct {
	Kind       string `json:"kind"`
	Schema     int    `json:"schema,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	// Digest is the digest of the table as it was pushed, the same as the
	// handshake's held state reports.
	Digest Digest `json:"digest,omitempty"`
	// PushedAt is the extclock at which the push was accepted.
	PushedAt *Time `json:"pushed_at,omitempty"`
	// Body is the table as the server holds it, canonical: what the
	// server does not keep (display contents) is absent, so its digest
	// may differ from Digest.
	Body json.RawMessage `json:"body,omitempty"`
}

// DecodePushedState decodes and validates an own-state answer.
func DecodePushedState(data []byte) (*PushedState, error) {
	if len(data) > MaxPushedStateBytes {
		return nil, malformed(objectPushedState, &fieldError{rule: RuleRange})
	}
	var p PushedState
	if err := decodeStrict(objectPushedState, data, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Held reports whether the server holds a table for the caller.
func (p *PushedState) Held() bool { return p.Generation != 0 }

// Validate checks the answer's rules: either only Kind, or every field.
func (p *PushedState) Validate() error {
	f := matches("kind", p.Kind, wordPattern)
	if f == nil && !p.Held() {
		switch {
		case p.Schema != 0:
			f = &fieldError{field: "schema", rule: RuleRelation}
		case p.Digest != "":
			f = &fieldError{field: "digest", rule: RuleRelation}
		case p.PushedAt != nil:
			f = &fieldError{field: "pushed_at", rule: RuleRelation}
		case len(p.Body) != 0:
			f = &fieldError{field: "body", rule: RuleRelation}
		}
	}
	if f == nil && p.Held() {
		f = first(
			inRange("schema", int64(p.Schema), 1, maxSafeInteger),
			inRange("generation", int64(min(p.Generation, maxSafeInteger+1)), 1, maxSafeInteger),
			matches("digest", string(p.Digest), digestPattern),
		)
		switch {
		case f != nil:
		case p.PushedAt == nil || p.PushedAt.IsZero():
			f = &fieldError{field: "pushed_at", rule: RuleRequired}
		case len(p.Body) == 0 || p.Body[0] != '{':
			f = &fieldError{field: "body", rule: RuleType}
		case len(p.Body) > MaxPushBytes:
			f = &fieldError{field: "body", rule: RuleRange}
		}
	}
	if f != nil {
		return wireInvalid(objectPushedState, f)
	}
	return nil
}

// Table decodes the held body at its kind and schema.
func (p *PushedState) Table() (Table, error) { return DecodeTable(p.Kind, p.Schema, p.Body) }
