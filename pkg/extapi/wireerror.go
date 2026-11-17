package extapi

const objectError = "error"

// Reasons classify a gwx/1 error for the caller's handling, independent of
// its code. The set is closed within protocol major 1.
const (
	ReasonInvalidArgument    = "invalid_argument"
	ReasonPermissionDenied   = "permission_denied"
	ReasonNotFound           = "not_found"
	ReasonFailedPrecondition = "failed_precondition"
	ReasonConflict           = "conflict"
	ReasonBusy               = "busy"
	ReasonUnavailable        = "unavailable"
	ReasonUnsupported        = "unsupported"
	ReasonCancelled          = "cancelled"
	ReasonDeadlineExceeded   = "deadline_exceeded"
	ReasonInternal           = "internal"
)

var reasons = []string{
	ReasonInvalidArgument, ReasonPermissionDenied, ReasonNotFound, ReasonFailedPrecondition,
	ReasonConflict, ReasonBusy, ReasonUnavailable, ReasonUnsupported, ReasonCancelled,
	ReasonDeadlineExceeded, ReasonInternal,
}

// HeaderProductError marks a plugin route's response body as a WireError.
const HeaderProductError = "Gwx-Product-Error"

// MaxErrorMessage bounds a WireError's English fallback.
const MaxErrorMessage = 2048

// WireError is the body of every gwx/1 error, in both directions, and of a
// plugin route's product error: a product error (ADR 0067) plus its reason.
type WireError struct {
	// Code is the stable UPPER_SNAKE code clients render.
	Code string `json:"code"`
	// Message is the English fallback.
	Message string `json:"message"`
	Params  Params `json:"params,omitempty"`
	Reason  string `json:"reason"`
	// RetryAfterMS suggests when to retry a busy or unavailable call.
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
}

// DecodeWireError decodes and validates an error body.
func DecodeWireError(data []byte) (*WireError, error) {
	var e WireError
	if err := decodeStrict(objectError, data, &e); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// Validate checks the error's rules.
func (e *WireError) Validate() error {
	f := first(
		matches("code", e.Code, codePattern),
		text("message", e.Message, MaxErrorMessage),
		e.Params.validate("params"),
		oneOf("reason", e.Reason, reasons...),
		inRange("retry_after_ms", e.RetryAfterMS, 0, 24*3600*1000),
	)
	if f == nil && e.RetryAfterMS > 0 && e.Reason != ReasonBusy && e.Reason != ReasonUnavailable {
		f = &fieldError{field: "retry_after_ms", rule: RuleRelation}
	}
	if f != nil {
		return wireInvalid(objectError, f)
	}
	return nil
}

// WireErrorOf turns an *Error into a WireError with the given reason.
func WireErrorOf(err *Error, reason string) WireError {
	return WireError{Code: err.Code, Message: err.Message, Params: Params(err.Params), Reason: reason}
}

// StatusForReason is the HTTP status a gwx/1 error with reason travels
// under, in both directions. The body decides; the status only lets a
// generic HTTP client tell success from failure and retry from refusal.
func StatusForReason(reason string) int {
	switch reason {
	case ReasonInvalidArgument:
		return 400
	case ReasonPermissionDenied:
		return 403
	case ReasonNotFound:
		return 404
	case ReasonFailedPrecondition:
		return 412
	case ReasonConflict:
		return 409
	case ReasonBusy:
		return 429
	case ReasonUnavailable:
		return 503
	case ReasonUnsupported:
		return 501
	case ReasonCancelled:
		return 499
	case ReasonDeadlineExceeded:
		return 504
	default:
		return 500
	}
}

// CapabilityMissing is the refusal of a plugin whose server offers no
// version of capability within [min, max].
func CapabilityMissing(capability string, min, max int) *Error {
	return newError(ErrorCapabilityMissing,
		map[string]any{"capability": capability, "min": min, "max": max},
		"the server offers no version of %s from %d to %d", capability, min, max)
}
