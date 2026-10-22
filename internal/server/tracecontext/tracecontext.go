// Package tracecontext is the gateway's W3C Trace Context (traceparent,
// tracestate): parsing what a client or an upstream gateway sent, minting the
// identifiers of a trace the gateway starts, and the trace-id ratio sampling
// decision. It is a leaf: the gateway (proxy) builds a Span per request, the
// request record (uxrecord) carries it, the request store keeps its trace id
// and the trace exporter (traceexport) turns it into an OTLP span.
//
// See docs/adr/0046-gateway-request-traces.md.
package tracecontext

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"strings"
	"time"
)

// TraceID is a W3C trace-id: 16 bytes, all zero is invalid.
type TraceID [16]byte

// SpanID is a W3C parent-id / OTLP span id: 8 bytes, all zero is invalid.
type SpanID [8]byte

// IsZero reports the invalid all-zero id, which also stands for "none".
func (t TraceID) IsZero() bool { return t == TraceID{} }

// String is the 32 lowercase hex digits of the header; "" for the zero id.
func (t TraceID) String() string {
	if t.IsZero() {
		return ""
	}
	return hex.EncodeToString(t[:])
}

// IsZero reports the invalid all-zero id, which also stands for "none".
func (s SpanID) IsZero() bool { return s == SpanID{} }

// String is the 16 lowercase hex digits of the header; "" for the zero id.
func (s SpanID) String() string {
	if s.IsZero() {
		return ""
	}
	return hex.EncodeToString(s[:])
}

// ParseTraceID reads a trace id typed or pasted by a person: 32 hex digits in
// either case, surrounding space ignored. The zero id is refused. The header
// itself is stricter (lowercase only, see ParseTraceparent).
func ParseTraceID(s string) (TraceID, bool) {
	var t TraceID
	s = strings.TrimSpace(s)
	if len(s) != 32 {
		return t, false
	}
	if _, err := hex.Decode(t[:], []byte(strings.ToLower(s))); err != nil {
		return TraceID{}, false
	}
	return t, !t.IsZero()
}

// Flags are the trace-flags byte. Only the bits defined here are forwarded.
type Flags byte

const (
	// FlagSampled says the caller may have recorded the trace and asks the
	// callee to record its part.
	FlagSampled Flags = 0x01
	// FlagRandom (Trace Context Level 2) says the rightmost 7 bytes of the
	// trace id are random, which is what ratio sampling relies on.
	FlagRandom Flags = 0x02
)

// known is every flag bit this package understands and forwards.
const known = FlagSampled | FlagRandom

// msBytes is how many leading bytes of a trace id the gateway mints hold the
// Unix millisecond it was minted at (48 bits last until year 10889).
const msBytes = 6

// NewTraceID mints a trace id for a trace the gateway starts: the Unix
// millisecond in the leading 6 bytes, then 10 random bytes. The time prefix
// lets the request store find the request without an index on the id (see
// Time); the random tail satisfies FlagRandom.
func NewTraceID(now time.Time) TraceID {
	var t TraceID
	ms := uint64(now.UnixMilli())
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ms)
	copy(t[:msBytes], b[8-msBytes:])
	binary.BigEndian.PutUint16(t[msBytes:], uint16(rand.Uint32()))
	binary.BigEndian.PutUint64(t[msBytes+2:], rand.Uint64())
	if t.IsZero() {
		t[15] = 1
	}
	return t
}

// Time decodes the leading millisecond of a trace id as NewTraceID writes
// it. Any id decodes to some instant; the caller decides whether it is
// plausible (an upstream's random id lands in the window of the last days
// with a probability of about 1e-6).
func (t TraceID) Time() time.Time {
	var b [8]byte
	copy(b[8-msBytes:], t[:msBytes])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(b[:])))
}

// NewSpanID mints a random span id. Identifiers need to be unique, not
// secret, so the runtime generator serves (no system call per request).
func NewSpanID() SpanID {
	var s SpanID
	binary.BigEndian.PutUint64(s[:], rand.Uint64())
	if s.IsZero() {
		s[7] = 1
	}
	return s
}

// Parent is a valid incoming traceparent (and its tracestate).
type Parent struct {
	Trace TraceID
	Span  SpanID
	Flags Flags
	// State is the tracestate to forward unchanged; empty when none came.
	State string
}

// ParseTraceparent reads the traceparent header values of a request
// (http.Header's slice, so a repeated header is seen) and the tracestate
// values. It follows the W3C processing model: exactly one traceparent;
// version ff is invalid; version 00 is exactly 55 characters; a later
// version may append fields after a '-' and is read by its first four;
// lowercase hex only; neither id all zeros. Anything else is no parent, and
// the tracestate goes with it.
func ParseTraceparent(traceparent, tracestate []string) (Parent, bool) {
	if len(traceparent) != 1 {
		return Parent{}, false
	}
	h := strings.TrimSpace(traceparent[0])
	if len(h) < 55 || h[2] != '-' || h[35] != '-' || h[52] != '-' {
		return Parent{}, false
	}
	version, ok := lowerHex(h[0:2])
	if !ok || version == "ff" || (version == "00" && len(h) != 55) || (len(h) > 55 && h[55] != '-') {
		return Parent{}, false
	}
	var p Parent
	if !decodeLower(p.Trace[:], h[3:35]) || !decodeLower(p.Span[:], h[36:52]) || p.Trace.IsZero() || p.Span.IsZero() {
		return Parent{}, false
	}
	var flags [1]byte
	if !decodeLower(flags[:], h[53:55]) {
		return Parent{}, false
	}
	p.Flags = Flags(flags[0]) & known
	p.State = joinState(tracestate)
	return p, true
}

// maxStateLen bounds the forwarded tracestate: the specification's 32
// members of at most 256 characters each would allow 8 KiB; vendors may cut
// a longer list, and a header that long is more likely abuse than a trace.
const maxStateLen = 512

// joinState combines repeated tracestate headers as the specification says
// (in order, comma separated) and drops a list too long to forward whole.
func joinState(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			parts = append(parts, v)
		}
	}
	state := strings.Join(parts, ",")
	if len(state) > maxStateLen {
		return ""
	}
	return state
}

func lowerHex(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return s, true
}

func decodeLower(dst []byte, s string) bool {
	if _, ok := lowerHex(s); !ok || len(s) != 2*len(dst) {
		return false
	}
	_, err := hex.Decode(dst, []byte(s))
	return err == nil
}

// Format is a version-00 traceparent value.
func Format(trace TraceID, span SpanID, flags Flags) string {
	var b [55]byte
	copy(b[:3], "00-")
	hex.Encode(b[3:35], trace[:])
	b[35] = '-'
	hex.Encode(b[36:52], span[:])
	b[52] = '-'
	hex.Encode(b[53:55], []byte{byte(flags & known)})
	return string(b[:])
}

// SampledByRatio is the trace-id ratio decision for a trace the gateway
// starts: the rightmost 7 bytes (56 bits, the random part FlagRandom
// promises) compared with ratio. Every service applying the same rule to the
// same trace agrees. ratio <= 0 samples nothing, >= 1 everything.
func SampledByRatio(t TraceID, ratio float64) bool {
	switch {
	case ratio <= 0:
		return false
	case ratio >= 1:
		return true
	}
	var b [8]byte
	copy(b[1:], t[9:])
	return binary.BigEndian.Uint64(b[:]) < uint64(ratio*float64(uint64(1)<<56))
}

// Span is the gateway's own span of one request: the trace it belongs to,
// its id, the caller's span when the request continued a trace, and whether
// it is sampled.
type Span struct {
	Trace  TraceID
	ID     SpanID
	Parent SpanID
	// Flags are what the gateway forwards: the sampled decision and, when
	// known, the random bit.
	Flags Flags
	// State is the caller's tracestate, forwarded unchanged.
	State string
}

// Sampled reports the span's sampling decision.
func (s Span) Sampled() bool { return s.Flags&FlagSampled != 0 }

// Begin starts the gateway's span for a request: a child of the incoming
// parent when there is a valid one (its sampled decision kept), otherwise
// the root of a new trace whose sampled decision is sampleRoot's (nil
// samples nothing).
func Begin(parent Parent, hasParent bool, now time.Time, sampleRoot func(TraceID) bool) Span {
	if hasParent {
		return Span{Trace: parent.Trace, ID: NewSpanID(), Parent: parent.Span, Flags: parent.Flags, State: parent.State}
	}
	s := Span{Trace: NewTraceID(now), ID: NewSpanID(), Flags: FlagRandom}
	if sampleRoot != nil && sampleRoot(s.Trace) {
		s.Flags |= FlagSampled
	}
	return s
}

// Traceparent is the header the gateway sends to the engine: the gateway's
// span as the engine's parent.
func (s Span) Traceparent() string { return Format(s.Trace, s.ID, s.Flags) }
