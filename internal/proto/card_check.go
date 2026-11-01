package proto

// Card checks on the wire. The ids, states and skip reasons are the ones in
// accelerator's card check catalog; proto only carries them.

// CapCardChecks: the agent reports each card's checks (GPUInfo.Checks) and
// answers RPCNodeCardCheckDetail and RPCNodeCardRecheck.
const CapCardChecks = "card.checks"

// Card check RPCs.
const (
	// RPCNodeCardCheckDetail returns the last run of one check on one card
	// (CardCheckDetailRequest → CardCheckDetail). The agent answers from
	// memory and runs nothing.
	RPCNodeCardCheckDetail = "node.card_check_detail"
	// RPCNodeCardRecheck runs the runtime check on one card now
	// (CardRecheckRequest → CardCheckDetail), and records its answer the way
	// the periodic check records its own.
	RPCNodeCardRecheck = "node.card_recheck"
)

// CardCheck is one check's state on a card, as a status report carries it.
// Short keys: every report carries one per card and check.
type CardCheck struct {
	ID     string `json:"id"`
	State  string `json:"s"`
	Reason string `json:"r,omitempty"`
	// AgeS is how many seconds before this report the check last ran, for a
	// check that does not run with every report; 0 for one that does.
	AgeS int32 `json:"age,omitempty"`
	// MS is how long that run took, in milliseconds, where it is measured.
	MS int32 `json:"ms,omitempty"`
	// Evidence is what a warning or failing run read, at most
	// CardCheckEvidenceBytes of it: the server records it with the
	// problem. A passing check carries none.
	Evidence []DoctorField `json:"e,omitempty"`
}

// CardCheckEvidenceBytes bounds the evidence a report carries for one check.
const CardCheckEvidenceBytes = 1024

// CardCheckDetailRequest names one check on one card (accelerator.CardIdent).
type CardCheckDetailRequest struct {
	Ident string `json:"ident"`
	Check string `json:"check"`
}

// CardRecheckRequest names the card to check now.
type CardRecheckRequest struct {
	Ident string `json:"ident"`
}

// CardCheckRun is one run of a check on a card.
type CardCheckRun struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// At is when the run finished, RFC 3339 in UTC.
	At         string `json:"at"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Probe is the command that shows what the check looks at, for a person
	// to run themselves.
	Probe string `json:"probe,omitempty"`
	// Evidence are the values the check read and decided on.
	Evidence []DoctorField `json:"evidence,omitempty"`
}

// CardCheckDetail is what the agent holds about one check on one card: its
// last run, and its last run that found a problem when that was an earlier one.
type CardCheckDetail struct {
	Ident string `json:"ident"`
	Check string `json:"check"`
	// Last is absent when the check has not run on the card since the agent
	// started.
	Last *CardCheckRun `json:"last,omitempty"`
	// LastRan is the latest run that did run (passed, warned or failed),
	// when Last is one that was skipped.
	LastRan *CardCheckRun `json:"last_ran,omitempty"`
	// LastProblem is the latest run that warned or failed, when it is not Last.
	LastProblem *CardCheckRun `json:"last_problem,omitempty"`
	// AgentStartedAt bounds what the agent can know: nothing before it.
	AgentStartedAt string `json:"agent_started_at"`
}
