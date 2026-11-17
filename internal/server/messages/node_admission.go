package messages

// Node admission codes (ADR 0067). An extension that refuses a new node or
// holds a node out of scheduling names its own code; these are what the
// console shows when it names none.
const (
	// ErrorNodeEnrollmentRefused: a registered enrollment guard refused a new
	// node, without a code of its own. Nothing was created.
	ErrorNodeEnrollmentRefused = "NODE_ENROLLMENT_REFUSED"
	// ErrorNodeSchedulingHeld: an extension holds the node out of
	// scheduling, without a code of its own, so an operator cannot turn
	// scheduling back on. What already runs there keeps running.
	ErrorNodeSchedulingHeld = "NODE_SCHEDULING_HELD"
)
