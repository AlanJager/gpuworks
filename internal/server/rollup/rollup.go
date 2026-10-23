// Package rollup accumulates the long-term statistics of operations analytics
// as they happen: per hour, the tokens each API key used on each deployment,
// each deployment's request outcomes and latencies, each card's utilization
// and the cards each deployment held (ADR 0055).
//
// Nothing here re-reads raw observations. The gateway hands every finished
// request over once, the metrics ingest every GPU sample once, a sampler the
// cards held once a minute; each adds a few numbers to the current hour's
// counters in memory. A flush takes what accumulated since the last one as a
// delta, which the stores add to their rows, so a flush that fails is merged
// back and retried, and a restart loses at most the unflushed delta. Hours are
// UTC; days are rolled up from hours in the statistics time zone by the
// stores.
package rollup

import "time"

// HourMS is the start of the UTC hour containing t, in Unix milliseconds.
func HourMS(t time.Time) int64 {
	ms := t.UnixMilli()
	return ms - ms%int64(time.Hour/time.Millisecond)
}

// HourOfMS is HourMS for a Unix-millisecond instant.
func HourOfMS(ms int64) int64 {
	return ms - ms%int64(time.Hour/time.Millisecond)
}

// Day is a calendar day in the statistics time zone as YYYYMMDD.
type Day int32

// DayOf is the day holding t in loc.
func DayOf(t time.Time, loc *time.Location) Day {
	y, m, d := t.In(loc).Date()
	return Day(y*10000 + int(m)*100 + d)
}

// Bounds are the day's start and end in loc, as Unix milliseconds; end is
// the next day's start.
func (d Day) Bounds(loc *time.Location) (startMS, endMS int64) {
	start := time.Date(int(d)/10000, time.Month(int(d)/100%100), int(d)%100, 0, 0, 0, 0, loc)
	return start.UnixMilli(), start.AddDate(0, 0, 1).UnixMilli()
}

// Next is the following day.
func (d Day) Next() Day {
	t := time.Date(int(d)/10000, time.Month(int(d)/100%100), int(d)%100+1, 12, 0, 0, 0, time.UTC)
	y, m, dd := t.Date()
	return Day(y*10000 + int(m)*100 + dd)
}

// HoursOf are the starts of the UTC hours a day of loc covers: an hour
// belongs to the day its start falls in, so in a zone off the hour (+5:30)
// the day's edges are off by that fraction.
func (d Day) HoursOf(loc *time.Location) (firstHourMS, endHourMS int64) {
	start, end := d.Bounds(loc)
	hour := int64(time.Hour / time.Millisecond)
	return ceilTo(start, hour), ceilTo(end, hour)
}

func ceilTo(v, step int64) int64 {
	if r := v % step; r != 0 {
		return v - r + step
	}
	return v
}
