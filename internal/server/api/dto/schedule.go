package dto

import "time"

// LifecycleSchedule is one standing order of a deployment's schedule: at
// Time (HH:MM) on Weekdays (0 Sunday … 6 Saturday) in Timezone (an IANA
// name; "" is the server's own zone), sleep at SleepLevel, wake, or run
// Replicas replicas. NextAt is its next occurrence, absent while disabled.
type LifecycleSchedule struct {
	ID         uint       `json:"id"`
	Action     string     `json:"action"`
	Weekdays   []int      `json:"weekdays"`
	Time       string     `json:"time"`
	Timezone   string     `json:"timezone"`
	SleepLevel int        `json:"sleep_level,omitempty"`
	Replicas   int        `json:"replicas,omitempty"`
	Enabled    bool       `json:"enabled"`
	NextAt     *time.Time `json:"next_at,omitempty"`
}

// LifecycleScheduleRequest creates or replaces one order of a deployment's
// schedule. Enabled defaults to true; SleepLevel is required for a sleep and
// Replicas for a replica count, and neither is accepted for another action.
type LifecycleScheduleRequest struct {
	Action     string `json:"action"`
	Weekdays   []int  `json:"weekdays"`
	Time       string `json:"time"`
	Timezone   string `json:"timezone"`
	SleepLevel *int   `json:"sleep_level,omitempty"`
	Replicas   *int   `json:"replicas,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

// IdleSleep is a deployment's idle sleep: after AfterSec without a user
// request it sleeps at Level; AfterSec 0 is off.
type IdleSleep struct {
	AfterSec int `json:"after_sec"`
	Level    int `json:"level"`
}

// WakeWaitResult answers a wake that waits for room (?wait=true) or makes
// room by putting the occupants to sleep (?yield=true): the sleepers it woke
// at once, those now waiting, and the occupants put to sleep.
type WakeWaitResult struct {
	Status  string   `json:"status"`
	Woken   []string `json:"woken"`
	Waiting []string `json:"waiting"`
	Yielded []string `json:"yielded"`
}
