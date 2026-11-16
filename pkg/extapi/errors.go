package extapi

import (
	"errors"
	"fmt"
)

// Codes of the failures this package reports. They are stable: a code is
// never renamed or reused for another meaning. The console renders each from
// its code and parameters; Error.Message is the English fallback.
const (
	// ErrorWireMalformed: the bytes are not one JSON value of the expected
	// shape (invalid JSON, a duplicate key, trailing data, a value of the
	// wrong JSON type, a non-integer or out-of-range number, invalid UTF-8 or
	// a lone surrogate escape, more bytes than the object's size limit).
	// Params: object, field, rule.
	ErrorWireMalformed = "PLUGIN_WIRE_MALFORMED"
	// ErrorWireUnknownField: a field this version of the object does not
	// define. Params: object, field.
	ErrorWireUnknownField = "PLUGIN_WIRE_UNKNOWN_FIELD"
	// ErrorWireInvalid: a field breaks a rule of its object. Params: object,
	// field, rule.
	ErrorWireInvalid = "PLUGIN_WIRE_INVALID"
	// ErrorWireNotCanonical: a signed payload is not canonical JSON. Params:
	// object.
	ErrorWireNotCanonical = "PLUGIN_WIRE_NOT_CANONICAL"
	// ErrorSchemaTooNew: an object at a version this build does not know.
	// Params: object, version, known.
	ErrorSchemaTooNew = "PLUGIN_SCHEMA_TOO_NEW"
	// ErrorManifestTooNew: a package manifest at a schema this build does not
	// know. Params: version, known, min_core_hint.
	ErrorManifestTooNew = "PLUGIN_MANIFEST_TOO_NEW"
	// ErrorProtocolUnsupported: the peer's protocol range does not include
	// the major this build speaks. Params: min, max, supported.
	ErrorProtocolUnsupported = "PLUGIN_PROTOCOL_UNSUPPORTED"
	// ErrorSignatureUntrusted: no signature on an envelope is by a key the
	// reader trusts, or a publisher certificate's grant (field) needs a
	// project root's signature and only other issuers signed it. Params:
	// object, and field for a certificate grant.
	ErrorSignatureUntrusted = "PLUGIN_SIGNATURE_UNTRUSTED"
	// ErrorSignatureInvalid: a signature by a trusted key does not verify.
	// Params: object, keyid.
	ErrorSignatureInvalid = "PLUGIN_SIGNATURE_INVALID"
	// ErrorStateRejected: a pushed table row breaks a rule of its kind.
	// Params: kind, field, rule, row.
	ErrorStateRejected = "PLUGIN_STATE_REJECTED"
	// ErrorStateTooLarge: a pushed table exceeds a size limit of its kind.
	// Params: kind, field, max.
	ErrorStateTooLarge = "PLUGIN_STATE_TOO_LARGE"
	// ErrorStateTooNew: persisted state carries a version marker newer than
	// this build knows; a downgraded server refuses to start rather than
	// drop or rewrite it. Params: marker, version, known, needs.
	ErrorStateTooNew = "PLUGIN_STATE_TOO_NEW"
)

// Error is a coded failure. Code is stable; Params are the values a client
// interpolates into the code's text; Message is the English fallback.
type Error struct {
	Code    string
	Params  map[string]any
	Message string
}

func (e *Error) Error() string { return e.Message }

// Is reports whether target is an *Error with the same code, so callers can
// test errors.Is(err, &extapi.Error{Code: extapi.ErrorStateRejected}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// CodeOf returns the code of the first *Error in err's chain, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func newError(code string, params map[string]any, format string, args ...any) *Error {
	return &Error{Code: code, Params: params, Message: fmt.Sprintf(format, args...)}
}

// Rules name what a field broke, in the rule parameter of ErrorWireInvalid,
// ErrorWireMalformed and ErrorStateRejected.
const (
	RuleRequired  = "required"  // missing or empty
	RuleGrammar   = "grammar"   // does not match its grammar
	RuleRange     = "range"     // a number or length outside its bounds
	RuleEnum      = "enum"      // not one of the closed set of values
	RuleDuplicate = "duplicate" // repeats a value that must be unique
	RuleOrder     = "order"     // a list that must be sorted is not
	RuleExclusive = "exclusive" // sets fields that exclude each other
	RuleRelation  = "relation"  // contradicts another field of the object
	RuleSyntax    = "syntax"    // not valid JSON or not valid UTF-8
	RuleType      = "type"      // a JSON value of the wrong type
	RuleNumber    = "number"    // not an integer, or out of the safe range
	RuleTrailing  = "trailing"  // data after the JSON value
)

// fieldError is a rule broken at a field path, before it is given the code
// of the object that was being validated.
type fieldError struct {
	field string
	rule  string
	row   string
}

func (f *fieldError) Error() string { return f.field + ": " + f.rule }

func wireInvalid(object string, f *fieldError) *Error {
	return newError(ErrorWireInvalid,
		map[string]any{"object": object, "field": f.field, "rule": f.rule},
		"%s: field %s breaks rule %s", object, f.field, f.rule)
}

func stateRejected(kind string, f *fieldError) *Error {
	return newError(ErrorStateRejected,
		map[string]any{"kind": kind, "field": f.field, "rule": f.rule, "row": f.row},
		"pushed %s: field %s breaks rule %s", kind, f.field, f.rule)
}
