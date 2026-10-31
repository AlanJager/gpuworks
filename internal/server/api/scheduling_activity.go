package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ErrSchedulingActivityUnavailable answers the routes that read the
// scheduling activity on a server built without it (a harness).
var ErrSchedulingActivityUnavailable = errors.New("scheduling activity is not available")

// SchedulingActivity is the scheduling activity's rollups as the API reads
// them (internal/server/schedactivity): counted as the events are written,
// the last 24 hours in memory and older hours and days in stats.db, so no
// read goes back to the event log.
type SchedulingActivity interface {
	// Activity is the events over [from, to) per bucket of step (0: the
	// service picks one that keeps the range within about 200 buckets, never
	// finer than the oldest part's figures), stacked by reason, with the
	// topN cards that changed hands most.
	Activity(ctx context.Context, from, to time.Time, step time.Duration, topN int) (dto.SchedulingActivity, error)
	// Deployments is each deployment's yields, wakes, awake time and the
	// span its instances held cards in over [from, to) (at most the last 24
	// hours); want, when not nil, limits it to some deployments.
	Deployments(from, to time.Time, want map[uint]bool) map[uint]store.DeploymentActivity
	// DeploymentsOnNodes is each deployment's yields, yields to it and wakes
	// over [from, to) (at most the last 24 hours) of the events whose cards
	// are on the named nodes (by UUID), each event once; want, when not nil,
	// limits it to some deployments.
	DeploymentsOnNodes(nodes []string, from, to time.Time, want map[uint]bool) map[uint]store.ResidencySwitches
}

// activityWindow is a window the activity answers for and its bucket.
type activityWindow struct {
	key        string
	span, step time.Duration
}

// activityWindows are the quick windows of GET /scheduling/activity; the UI
// offers the same keys (ui/src/timeRange.ts, SCHEDULING_QUICK_RANGES).
var activityWindows = []activityWindow{
	{"15m", 15 * time.Minute, time.Minute},
	{"1h", time.Hour, time.Minute},
	{"6h", 6 * time.Hour, 5 * time.Minute},
	{"24h", 24 * time.Hour, 15 * time.Minute},
	{"7d", 7 * 24 * time.Hour, time.Hour},
}

const (
	// activityDefaultWindow is the window when none is asked for.
	activityDefaultWindow = "24h"
	// activityTopCards is how many cards an answer lists.
	activityTopCards = 20
	// activityMaxRange bounds a custom range: the longest day retention.
	activityMaxRange = 1825 * 24 * time.Hour
	// activityTTL is how long an answer is kept.
	activityTTL = 30 * time.Second
	// activityCacheSize bounds the answers kept; past it the cache starts
	// over.
	activityCacheSize = 64
)

// activityCache keeps the last answer of each window or custom range.
type activityCache struct {
	mu      sync.Mutex
	byRange map[string]activityEntry
	reads   atomic.Int64
}

type activityEntry struct {
	out dto.SchedulingActivity
	at  time.Time
}

// schedulingActivity answers GET /scheduling/activity: the scheduling events
// over a window (window=15m|1h|6h|24h|7d, 24h when neither it nor since is
// given) or a custom range (since, until: RFC 3339, until defaulting to now)
// per bucket, stacked by reason, and the cards that changed hands most. It
// only reads, and keeps each answer for activityTTL.
func (a *apiHandlers) schedulingActivity(w http.ResponseWriter, r *http.Request) {
	if a.d.SchedulingActivity == nil {
		writeErr(w, http.StatusNotFound, ErrSchedulingActivityUnavailable)
		return
	}
	invalid := func(field, reason string) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+reason, map[string]any{"field": field})
	}
	q := r.URL.Query()
	window := strings.TrimSpace(q.Get("window"))
	rawSince, rawUntil := strings.TrimSpace(q.Get("since")), strings.TrimSpace(q.Get("until"))
	now := time.Now().UTC().Truncate(time.Second)
	var from, to time.Time
	var step time.Duration
	key := window
	switch {
	case window != "" && (rawSince != "" || rawUntil != ""):
		invalid("window", "is not asked for with since or until")
		return
	case rawSince == "" && rawUntil != "":
		invalid("since", "is required with until")
		return
	case rawSince == "":
		if window == "" {
			window, key = activityDefaultWindow, activityDefaultWindow
		}
		spec, ok := activityWindowOf(window)
		if !ok {
			invalid("window", "is one of "+activityWindowKeys())
			return
		}
		from, to, step = now.Add(-spec.span), now, spec.step
	default:
		var err error
		if from, err = time.Parse(time.RFC3339, rawSince); err != nil {
			invalid("since", "is an RFC 3339 time")
			return
		}
		to = now
		if rawUntil != "" {
			if to, err = time.Parse(time.RFC3339, rawUntil); err != nil {
				invalid("until", "is an RFC 3339 time")
				return
			}
		}
		from, to = from.UTC(), to.UTC()
		if to.After(now) {
			to = now
		}
		if !to.After(from) {
			invalid("until", "is after since and not after now")
			return
		}
		if to.Sub(from) > activityMaxRange {
			invalid("since", "is at most 1825 days before until")
			return
		}
		key = from.Format(time.RFC3339) + "/" + rawUntil
	}
	out, err := a.cachedActivity(r.Context(), key, from, to, step)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func activityWindowOf(key string) (activityWindow, bool) {
	for _, w := range activityWindows {
		if w.key == key {
			return w, true
		}
	}
	return activityWindow{}, false
}

func activityWindowKeys() string {
	keys := make([]string, len(activityWindows))
	for i, w := range activityWindows {
		keys[i] = w.key
	}
	return strings.Join(keys, ", ")
}

// cachedActivity is the answer for a range, kept for activityTTL: a quick
// window slides with the clock, so a kept answer is at most that old.
func (a *apiHandlers) cachedActivity(ctx context.Context, key string, from, to time.Time, step time.Duration) (dto.SchedulingActivity, error) {
	c := &a.overview.activity
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.byRange[key]; ok && time.Since(entry.at) < activityTTL {
		return entry.out, nil
	}
	out, err := a.d.SchedulingActivity.Activity(ctx, from, to, step, activityTopCards)
	if err != nil {
		return out, err
	}
	if c.byRange == nil || len(c.byRange) >= activityCacheSize {
		c.byRange = map[string]activityEntry{}
	}
	c.byRange[key] = activityEntry{out: out, at: time.Now().UTC()}
	c.reads.Add(1)
	return out, nil
}
