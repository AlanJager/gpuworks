package uxrecord

// Result is how a finished request turned out, in the terms the request
// history filters by. Every request has exactly one: an error status wins
// over how the copy ended, and a reply the engine stopped at its length
// limit is told apart from one that finished on its own.
type Result string

const (
	// ResultCompleted is a 2xx reply copied whole that the engine ended on
	// its own (or with no finish reason at all).
	ResultCompleted Result = "completed"
	// ResultLength is a 2xx reply copied whole that the engine stopped at
	// the request's output limit (finish_reason "length").
	ResultLength Result = "length"
	// ResultClientClosed is a 2xx reply the client stopped reading.
	ResultClientClosed Result = "client_closed"
	// ResultInterrupted is a 2xx reply whose connection was cut before its
	// end.
	ResultInterrupted Result = "interrupted"
	// ResultError is a request the instance never answered or answered with
	// a non-2xx status.
	ResultError Result = "error"
)

// Results lists every result, in the order a filter offers them.
func Results() []Result {
	return []Result{ResultCompleted, ResultLength, ResultClientClosed, ResultInterrupted, ResultError}
}

// FinishLength is the finish reason of a reply cut at its output limit.
const FinishLength = "length"

// ResultOf classifies a finished request.
func ResultOf(r Request) Result {
	switch {
	case r.Outcome == OutcomeUpstreamError || r.Status < 200 || r.Status >= 300:
		return ResultError
	case r.Outcome == OutcomeClientClosed:
		return ResultClientClosed
	case r.Outcome == OutcomeTruncated:
		return ResultInterrupted
	case r.FinishReason == FinishLength:
		return ResultLength
	}
	return ResultCompleted
}

// ParseResult reads a result's name.
func ParseResult(name string) (Result, bool) {
	for _, r := range Results() {
		if string(r) == name {
			return r, true
		}
	}
	return "", false
}
