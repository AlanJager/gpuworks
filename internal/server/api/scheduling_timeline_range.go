package api

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

const (
	// timelineMaxRange bounds a custom range of a node's timeline.
	timelineMaxRange = 7 * 24 * time.Hour
	// timelineSkew is how far past now an until may be: a browser's clock a
	// little ahead of the server's. The range then ends at now.
	timelineSkew = time.Minute
	// timelineSwitchesSpan is how far back the deployments' yields and wakes
	// are known: the scheduling activity's memory (schedactivity.Span).
	timelineSwitchesSpan = 24 * time.Hour
	// timelineSettle is how long before now a custom range must have ended
	// for its answer to be kept: events written a moment ago may still be
	// landing in it.
	timelineSettle = time.Minute
)

// timelineRange is what a request asked a timeline for: one of the quick
// windows, which ends now, or a custom range.
type timelineRange struct {
	// win is the quick window (zero for a custom range).
	win    timelineWindow
	custom bool
	from   time.Time
	to     time.Time
}

// quickRange is the window ending at now.
func quickRange(win timelineWindow, now time.Time) timelineRange {
	return timelineRange{win: win, from: now.Add(-win.span), to: now}
}

// wire is the range as the answer names it.
func (r timelineRange) wire() dto.TimelineWindow {
	if !r.custom {
		return r.win.wire(r.to)
	}
	span := r.to.Sub(r.from)
	return dto.TimelineWindow{
		From: r.from, To: r.to, Hours: int(span / time.Hour), Key: dto.TimelineWindowCustom,
		Seconds: int64(span / time.Second), Custom: true,
	}
}

// until is where the store's reads stop: the end of a custom range, or zero
// for a window that runs to now.
func (r timelineRange) until() time.Time {
	if r.custom {
		return r.to
	}
	return time.Time{}
}

// isPast says the range ended long enough ago for its lanes to be final.
func (r timelineRange) isPast(now time.Time) bool {
	return r.custom && !r.to.After(now.Add(-timelineSettle))
}

// switchesKnown says the deployments' yields and wakes are known over the
// range: the activity's memory reaches back to its start.
func (r timelineRange) switchesKnown(now time.Time) bool {
	return !r.from.Before(now.Add(-timelineSwitchesSpan))
}

// badField is a request parameter that is not valid and why.
type badField struct{ field, reason string }

// timelineRangeOf reads the range a request names: window (one of windows,
// def when none is given) or hours (the older form of window, 6 or 24), or a
// custom range, since and until (RFC 3339, until defaulting to now), which
// only a node's timeline has and which replaces window and hours.
func timelineRangeOf(q url.Values, windows []timelineWindow, def timelineWindow, custom bool, now time.Time) (timelineRange, *badField) {
	window, hours := strings.TrimSpace(q.Get("window")), strings.TrimSpace(q.Get("hours"))
	rawSince, rawUntil := strings.TrimSpace(q.Get("since")), strings.TrimSpace(q.Get("until"))
	asked := rawSince != "" || rawUntil != ""
	win := def
	if hours != "" {
		n, ok := hoursOf(hours)
		if !ok {
			return timelineRange{}, &badField{"hours", "is one of " + joinInts(timelineHours)}
		}
		win, _ = windowOf(windows, n)
	}
	if window != "" {
		var ok bool
		if win, ok = windowOf(windows, window); !ok {
			return timelineRange{}, &badField{"window", "is one of " + windowKeys(windows)}
		}
	}
	if !asked {
		return quickRange(win, now), nil
	}
	switch {
	case !custom:
		field := "since"
		if rawSince == "" {
			field = "until"
		}
		return timelineRange{}, &badField{field, "is not asked for with sharing"}
	case window != "":
		return timelineRange{}, &badField{"window", "is not asked for with since or until"}
	case hours != "":
		return timelineRange{}, &badField{"hours", "is not asked for with since or until"}
	case rawSince == "":
		return timelineRange{}, &badField{"since", "is required with until"}
	}
	from, err := time.Parse(time.RFC3339, rawSince)
	if err != nil {
		return timelineRange{}, &badField{"since", "is an RFC 3339 time"}
	}
	to := now
	if rawUntil != "" {
		if to, err = time.Parse(time.RFC3339, rawUntil); err != nil {
			return timelineRange{}, &badField{"until", "is an RFC 3339 time"}
		}
	}
	from, to = from.UTC().Truncate(time.Second), to.UTC().Truncate(time.Second)
	if to.After(now.Add(timelineSkew)) {
		return timelineRange{}, &badField{"until", "is not after now"}
	}
	if to.After(now) {
		to = now
	}
	if !to.After(from) {
		return timelineRange{}, &badField{"until", "is after since"}
	}
	if to.Sub(from) > timelineMaxRange {
		return timelineRange{}, &badField{"since", "is at most 7 days before until"}
	}
	return timelineRange{custom: true, from: from, to: to}, nil
}

// hoursOf is the window key the older hours parameter names ("6" is "6h"),
// and whether it is one of timelineHours.
func hoursOf(raw string) (string, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || !containsInt(timelineHours, n) {
		return "", false
	}
	return strconv.Itoa(n) + "h", true
}
