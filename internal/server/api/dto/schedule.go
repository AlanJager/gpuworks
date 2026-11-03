package dto

import "time"

// LifecycleSchedule is one standing order of a deployment's schedule: at
// Time (HH:MM) on Weekdays (0 Sunday … 6 Saturday) in Timezone (an IANA
// name; "" is the server's own zone), sleep at SleepLevel, wake, or run
// Replicas replicas. NextAt is its next occurrence, absent while disabled.
type LifecycleSchedule struct {
	ID         uint   `json:"id"`
	Action     string `json:"action"`
	Weekdays   []int  `json:"weekdays"`
	Time       string `json:"time"`
	Timezone   string `json:"timezone"`
	SleepLevel int    `json:"sleep_level,omitempty"`
	// Replicas is stated for a replicas order (0 included: a PD role the
	// mode lets be empty) and absent for any other. Role is the PD role it
	// scales, absent for a deployment that is not PD.
	Replicas *int       `json:"replicas,omitempty"`
	Role     string     `json:"role,omitempty"`
	Enabled  bool       `json:"enabled"`
	NextAt   *time.Time `json:"next_at,omitempty"`
}

// LifecycleScheduleRequest creates or replaces one order of a deployment's
// schedule. Enabled defaults to true; SleepLevel is required for a sleep and
// Replicas for a replica count, and neither is accepted for another action.
// Role names the PD role a replica count scales: required for a PD deployment,
// refused for any other.
type LifecycleScheduleRequest struct {
	Action     string `json:"action"`
	Weekdays   []int  `json:"weekdays"`
	Time       string `json:"time"`
	Timezone   string `json:"timezone"`
	SleepLevel *int   `json:"sleep_level,omitempty"`
	Replicas   *int   `json:"replicas,omitempty"`
	Role       string `json:"role,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

// WakeWaitResult answers a wake that waits for room (?wait=true) or makes
// room by putting the occupants to sleep (?yield=true): the sleepers it woke
// at once, those now waiting, and the occupants put to sleep. YieldedHeld are
// the yielded occupants whose sleep is held, because their yield policy is
// never: a request does not wake them. The others a request wakes
// (planning.OperatorSleepWakesOnRequest).
type WakeWaitResult struct {
	Status      string   `json:"status"`
	Woken       []string `json:"woken"`
	Waiting     []string `json:"waiting"`
	Yielded     []string `json:"yielded"`
	YieldedHeld []string `json:"yielded_held"`
}
