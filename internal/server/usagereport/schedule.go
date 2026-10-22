package usagereport

import (
	"hash/fnv"
	"time"
)

// DayLayout is how a report names the UTC day it covers.
const DayLayout = "2006-01-02"

// A day's report is sent the next day, 00:30 UTC plus a spread of up to six
// hours taken from the installation ID, so installations do not all report at
// the same minute and one installation reports at the same time every day.
const (
	dueAfterMidnight = 30 * time.Minute
	dueSpread        = 6 * time.Hour
)

// Next says which UTC day the next report covers and when it is due, given
// the last day reported ("" for none). Only the latest whole day is ever
// pending: a server that was down for a week reports yesterday, not the week.
func Next(installID, lastDay string, now time.Time) (day, due time.Time) {
	today := now.UTC().Truncate(24 * time.Hour)
	day = today.AddDate(0, 0, -1)
	if last, err := time.Parse(DayLayout, lastDay); err == nil && !last.Before(day) {
		day = today
	}
	return day, day.AddDate(0, 0, 1).Add(dueAfterMidnight + spread(installID))
}

// Window is the time span a day's report covers: [day, day+24h).
func Window(day time.Time) (since, until time.Time) {
	since = day.UTC().Truncate(24 * time.Hour)
	return since, since.AddDate(0, 0, 1)
}

func spread(installID string) time.Duration {
	h := fnv.New64a()
	h.Write([]byte(installID))
	return time.Duration(h.Sum64() % uint64(dueSpread/time.Minute) * uint64(time.Minute))
}
