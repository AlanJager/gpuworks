package messages

// Product error codes of the scheduling center: a deployment's schedule and
// waiting wakes.
const (
	// ErrorScheduleReplicasManualPlacement: a schedule sets the replica count
	// of a multi-node or PD deployment whose replicas the operator placed by
	// hand; only one the platform places can grow by a count.
	ErrorScheduleReplicasManualPlacement = "SCHEDULE_REPLICAS_MANUAL_PLACEMENT"
	// ErrorScheduleInvalid: a schedule order is malformed ({field}, {reason});
	// a replicas order of a PD deployment that names no role of its mode, or
	// any other deployment's that names one, is {field: "role"}.
	ErrorScheduleInvalid = "SCHEDULE_INVALID"
	// ErrorScheduleTimezoneUnknown: the server does not know the schedule's
	// time zone ({timezone}).
	ErrorScheduleTimezoneUnknown = "SCHEDULE_TIMEZONE_UNKNOWN"
	// ErrorWakeYieldBlocked: a yielding wake cannot make room because an
	// occupant cannot be put to sleep ({occupants}).
	ErrorWakeYieldBlocked = "WAKE_YIELD_BLOCKED"
)
