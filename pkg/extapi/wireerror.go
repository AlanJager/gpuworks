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
