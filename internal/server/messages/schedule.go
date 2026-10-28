package messages

// Product error codes of the scheduling center: a deployment's schedule, idle
// sleep and waiting wakes.
const (
	// ErrorScheduleReplicasShape: a schedule sets the replica count of a
	// multi-node or PD deployment, whose replicas name their nodes.
	ErrorScheduleReplicasShape = "SCHEDULE_REPLICAS_SHAPE"
	// ErrorScheduleInvalid: a schedule order is malformed ({field}, {reason}).
	ErrorScheduleInvalid = "SCHEDULE_INVALID"
	// ErrorScheduleTimezoneUnknown: the server does not know the schedule's
	// time zone ({timezone}).
	ErrorScheduleTimezoneUnknown = "SCHEDULE_TIMEZONE_UNKNOWN"
	// ErrorIdleSleepInvalid: an idle sleep setting is out of range ({field}).
	ErrorIdleSleepInvalid = "IDLE_SLEEP_INVALID"
	// ErrorWakeYieldBlocked: a yielding wake cannot make room because an
	// occupant cannot be put to sleep ({occupants}).
	ErrorWakeYieldBlocked = "WAKE_YIELD_BLOCKED"
)
