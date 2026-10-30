// Package automation is the scheduling center's hand: it carries out the
// orders operators gave in advance - a deployment's schedule, its auto sleep,
// a wake waiting for room - through the same sleep, wake and replica orders
// an operator gives by hand. It decides nothing an operator did not ask for:
// there is no preemption and no load-driven scaling.
//
// It reads and acts through the Source and Actions interfaces, so the
// decisions are tested with a fake clock and fakes, and the app wires them to
// the store, the ledger and the gateway.
package automation

import (
	"time"
)

// Schedule is one standing order of a deployment's schedule.
type Schedule struct {
	ID           uint
	DeploymentID uint
	Action       string // sleep | wake | replicas
	// Weekdays has bit 0 for Sunday … bit 6 for Saturday.
	Weekdays int
	// Minute is minutes after midnight in Location.
	Minute     int
	Location   *time.Location
	SleepLevel int
	Replicas   int
	// Role is the PD role a replicas order scales; empty for a deployment
	// that is not PD.
	Role string
	// LastFiredAt is the scheduled time of the newest occurrence handled.
	LastFiredAt time.Time
}

// AllWeekdays is every day of the week.
const AllWeekdays = 1<<7 - 1

// LatestOccurrence is the newest time at or before now the schedule names,
// and false when it names none in the week before now (no weekday set).
func (s Schedule) LatestOccurrence(now time.Time) (time.Time, bool) {
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}
	local := now.In(loc)
	for back := 0; back <= 7; back++ {
		day := local.AddDate(0, 0, -back)
		if s.Weekdays&(1<<int(day.Weekday())) == 0 {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), s.Minute/60, s.Minute%60, 0, 0, loc)
		if !at.After(local) {
			return at, true
		}
	}
	return time.Time{}, false
}

// Occurrences are the times in (from, to] the schedule names, in order.
func (s Schedule) Occurrences(from, to time.Time) []time.Time {
	loc := s.Location
	if loc == nil {
		loc = time.Local
	}
	var out []time.Time
	start := from.In(loc)
	for day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc); !day.After(to); day = day.AddDate(0, 0, 1) {
		if s.Weekdays&(1<<int(day.Weekday())) == 0 {
			continue
		}
		at := time.Date(day.Year(), day.Month(), day.Day(), s.Minute/60, s.Minute%60, 0, 0, loc)
		if at.After(from) && !at.After(to) {
			out = append(out, at)
		}
	}
	return out
}

// due is the occurrence the schedule should act on now: the newest one not
// handled yet. stale says it is older than the catch-up window, so it is only
// marked handled; a server that was down for days does not replay a stale
// order on start.
func (s Schedule) due(now time.Time, catchUp time.Duration) (at time.Time, ok, stale bool) {
	at, ok = s.LatestOccurrence(now)
	if !ok || !at.After(s.LastFiredAt) {
		return time.Time{}, false, false
	}
	return at, true, now.Sub(at) > catchUp
}
