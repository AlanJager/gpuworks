package api

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

const (
	// timelinePastTTL is how long the answer of a custom range in the past
	// is kept. The lanes of such a range do not change; the rest of the
	// answer (deployments' policy, whether they still exist, where they run
	// now) does, slowly.
	timelinePastTTL = 5 * time.Minute
	// timelineQuickTTL is how long the answer of a quick window (15m, 1h,
	// 6h, 24h) is kept, as sharingTTL keeps the fleet's stats: viewers
	// polling every minute share one read of the event log. What in the
	// answer is "now" - the window's end (a lane still open ends there, so
	// it stops up to this much short of now), the cards where the
	// deployments live, the yields and wakes counted so far, the nodes and
	// their GPUs - is that much behind; none of it is read at a finer
	// grain than the minute the page polls at.
	timelineQuickTTL = 30 * time.Second
	// timelineCacheSize bounds the answers kept; when full, expired ones go
	// first and then the oldest. An answer with more than
	// timelineCacheSegments elements (segments and summaries) is not kept,
	// which bounds what the cache holds.
	timelineCacheSize     = 32
	timelineCacheSegments = 20000
)

// timelineCache keeps the answers of the quick windows (for timelineQuickTTL)
// and of custom ranges that ended in the past (for timelinePastTTL), per range
// and page of nodes: viewers of the same page, or several looking at the same
// incident, share one read of the event log.
type timelineCache struct {
	mu       sync.Mutex
	byKey    map[string]timelineEntry
	inflight map[string]*timelineFlight
	reads    atomic.Int64
}

type timelineEntry struct {
	out dto.SchedulingTimeline
	at  time.Time
	ttl time.Duration
}

// timelineFlight is a read in progress that later askers of its key wait for.
type timelineFlight struct {
	done chan struct{}
	out  dto.SchedulingTimeline
	err  error
}

// cacheKey names the range and the page of nodes an answer is for. A quick
// window is named by its key: the answer slides with the clock, and the
// entry's age bounds how far.
func (r timelineRange) cacheKey(uuids []string) string {
	page := strings.Join(uuids, ",")
	if !r.custom {
		return "window/" + r.win.key + "/" + page
	}
	return r.from.Format(time.RFC3339) + "/" + r.to.Format(time.RFC3339) + "/" + page
}

// ttl is how long the answer of the range is kept, zero when it is not: a
// custom range that ended a moment ago is still landing events.
func (r timelineRange) ttl(now time.Time) time.Duration {
	switch {
	case !r.custom:
		return timelineQuickTTL
	case r.isPast(now):
		return timelinePastTTL
	}
	return 0
}

// cachedResidencyTimeline is residencyTimeline, kept for the range's ttl; a
// custom range that ended a moment ago is read every time. Askers of one key
// that arrive while it is being read wait for that read.
func (a *apiHandlers) cachedResidencyTimeline(uuids []string, rng timelineRange, now time.Time) (dto.SchedulingTimeline, error) {
	ttl := rng.ttl(now)
	if ttl == 0 {
		return a.residencyTimeline(uuids, rng, now)
	}
	c := &a.overview.timeline
	key := rng.cacheKey(uuids)
	c.mu.Lock()
	if entry, ok := c.byKey[key]; ok && time.Since(entry.at) < entry.ttl {
		c.mu.Unlock()
		return entry.out, nil
	}
	if flight := c.inflight[key]; flight != nil {
		c.mu.Unlock()
		<-flight.done
		return flight.out, flight.err
	}
	flight := &timelineFlight{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = map[string]*timelineFlight{}
	}
	c.inflight[key] = flight
	c.mu.Unlock()

	flight.out, flight.err = a.residencyTimeline(uuids, rng, now)
	c.reads.Add(1)
	c.mu.Lock()
	delete(c.inflight, key)
	if flight.err == nil && timelineSegments(flight.out) <= timelineCacheSegments {
		c.keep(key, timelineEntry{out: flight.out, at: time.Now().UTC(), ttl: ttl})
	}
	c.mu.Unlock()
	close(flight.done)
	return flight.out, flight.err
}

// keep stores an entry, making room first when the cache is full: the
// expired ones go, and if that is not enough, the oldest. Called with mu held.
func (c *timelineCache) keep(key string, entry timelineEntry) {
	if c.byKey == nil {
		c.byKey = map[string]timelineEntry{}
	}
	if _, replace := c.byKey[key]; !replace && len(c.byKey) >= timelineCacheSize {
		for k, e := range c.byKey {
			if time.Since(e.at) >= e.ttl {
				delete(c.byKey, k)
			}
		}
		for len(c.byKey) >= timelineCacheSize {
			oldest := ""
			for k, e := range c.byKey {
				if oldest == "" || e.at.Before(c.byKey[oldest].at) {
					oldest = k
				}
			}
			delete(c.byKey, oldest)
		}
	}
	c.byKey[key] = entry
}

// timelineSegments is how many elements - raw segments and summaries - an
// answer lists.
func timelineSegments(out dto.SchedulingTimeline) int {
	n := 0
	for _, node := range out.Nodes {
		for _, gpu := range node.GPUs {
			n += len(gpu.Segments) + len(gpu.Summary)
		}
	}
	return n
}
