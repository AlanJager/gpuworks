package api

import (
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// timelinePastTTL is how long the answer of a range in the past is kept.
	// The lanes of such a range do not change; the rest of the answer
	// (deployments' policy, whether they still exist, where they run now)
	// does, slowly.
	timelinePastTTL = 5 * time.Minute
	// timelineCacheSize bounds the answers kept; past it the cache starts
	// over. An answer with more than timelineCacheSegments segments is not
	// kept, which bounds what the cache holds.
	timelineCacheSize     = 16
	timelineCacheSegments = 20000
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

// timelineCache keeps the answers of custom ranges that ended in the past,
// per range and page of nodes: a viewer reading a past range once and
// coming back to it, or several looking at the same incident, share one read
// of the event log.
type timelineCache struct {
	mu    sync.Mutex
	byKey map[string]timelineEntry
	reads atomic.Int64
}

type timelineEntry struct {
	out dto.SchedulingTimeline
	at  time.Time
}

// cachedResidencyTimeline is residencyTimeline, kept for timelinePastTTL when
// the range is a custom one in the past. A quick window slides with the clock
// and is read every time.
func (a *apiHandlers) cachedResidencyTimeline(uuids []string, rng timelineRange, now time.Time) (dto.SchedulingTimeline, error) {
	if !rng.isPast(now) {
		return a.residencyTimeline(uuids, rng, now)
	}
	c := &a.overview.timeline
	key := rng.from.Format(time.RFC3339) + "/" + rng.to.Format(time.RFC3339) + "/" + strings.Join(uuids, ",")
	c.mu.Lock()
	if entry, ok := c.byKey[key]; ok && time.Since(entry.at) < timelinePastTTL {
		c.mu.Unlock()
		return entry.out, nil
	}
	c.mu.Unlock()
	out, err := a.residencyTimeline(uuids, rng, now)
	if err != nil {
		return out, err
	}
	c.reads.Add(1)
	if timelineSegments(out) > timelineCacheSegments {
		return out, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil || len(c.byKey) >= timelineCacheSize {
		c.byKey = map[string]timelineEntry{}
	}
	c.byKey[key] = timelineEntry{out: out, at: time.Now().UTC()}
	return out, nil
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

// timelineSegments is how many segments an answer lists.
func timelineSegments(out dto.SchedulingTimeline) int {
	n := 0
	for _, node := range out.Nodes {
		for _, gpu := range node.GPUs {
			n += len(gpu.Segments)
		}
	}
	return n
}
