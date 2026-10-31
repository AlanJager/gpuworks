package api

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The stats of the deployments that share GPUs, as the fleet's sharing stats
// and a node page's timeline both answer them (docs/reference/api.md,
// section 驻留时间线).

// interval is a stretch of time.
type interval struct{ from, to time.Time }

// cover is the stretch from the earliest start to the latest end of i and
// [from, to]; the zero interval covers nothing.
func (i interval) cover(from, to time.Time) interval {
	if i.from.IsZero() {
		return interval{from, to}
	}
	if from.Before(i.from) {
		i.from = from
	}
	if to.After(i.to) {
		i.to = to
	}
	return i
}

// seconds is the interval's length, rounded, and 0 when it is empty.
func (i interval) seconds() int64 {
	if !i.to.After(i.from) {
		return 0
	}
	return int64(i.to.Sub(i.from).Seconds() + 0.5)
}

// unionSeconds is the time the intervals cover together.
func unionSeconds(spans []interval) int64 {
	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })
	var total time.Duration
	var end time.Time
	for _, span := range spans {
		start := span.from
		if start.Before(end) {
			start = end
		}
		if span.to.After(start) {
			total += span.to.Sub(start)
			end = span.to
		}
	}
	return int64(total.Seconds() + 0.5)
}

// switchesTTL is how long every deployment's switch counts over a window are
// kept. They are read from every automation sleep and wake of the window
// across the fleet, so the node pages a viewer opens and the fleet's stats
// share one read rather than one per page.
const switchesTTL = 30 * time.Second

// switchCache keeps the last switch counts of each window. The maps are
// shared by the answers built from them and are never changed once kept.
type switchCache struct {
	mu      sync.Mutex
	byHours map[int]switchEntry
	reads   atomic.Int64
}

type switchEntry struct {
	counts map[uint]store.ResidencySwitches
	at     time.Time
}

// switchCounts is every deployment's switches over the last hours before now:
// the one count both the fleet's sharing stats and a node page's timeline
// read, so the two agree. It is kept for switchesTTL.
func (a *apiHandlers) switchCounts(now time.Time, hours int) (map[uint]store.ResidencySwitches, error) {
	c := &a.overview.switches
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.byHours[hours]; ok && time.Since(entry.at) < switchesTTL {
		return entry.counts, nil
	}
	counts := map[uint]store.ResidencySwitches{}
	first, ok, err := a.d.Store.ResidencyWindowStart(now.Add(-time.Duration(hours) * time.Hour))
	if err != nil {
		return nil, err
	}
	if ok {
		if counts, err = a.d.Store.ResidencySwitchCounts(first); err != nil {
			return nil, err
		}
	}
	if c.byHours == nil {
		c.byHours = map[int]switchEntry{}
	}
	c.byHours[hours] = switchEntry{counts: counts, at: time.Now().UTC()}
	c.reads.Add(1)
	return counts, nil
}

// deletedSharers are the deployments, among those that took part in a yield
// over the window and are not in known, that have been deleted since: they
// shared cards then, so the fleet's stats list them as well.
func (a *apiHandlers) deletedSharers(known []uint, counts map[uint]store.ResidencySwitches) ([]uint, error) {
	listed := map[uint]bool{}
	for _, id := range known {
		listed[id] = true
	}
	var candidates []uint
	for id, c := range counts {
		if !listed[id] && c.TookPartInYields() {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	names, err := a.d.Store.ResidencyDeployments(candidates)
	if err != nil {
		return nil, err
	}
	var out []uint
	for _, id := range candidates {
		if names[id].Deleted {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// listedInSharing says a deployment has a row in the sharing stats: one that
// exists and yields at all (any policy but never), or a deleted one that
// gave its cards up or was given another's over the window (its policy is
// not kept, so what it did is what says it shared).
func listedInSharing(info store.ResidencyDeployment, c store.ResidencySwitches) bool {
	if info.Deleted {
		return c.TookPartInYields()
	}
	return info.Yield != yieldpolicy.Never
}

// presence is the part of the window [from, now] a deployment existed in:
// from its creation, or its first instance's first segment when that is
// earlier (a deleted deployment's creation is not kept), to its deletion, or
// to its last segment when that is not known, and to now while it exists.
// seen spans its segments, which lie inside the window.
func presence(info store.ResidencyDeployment, seen interval, from, now time.Time) interval {
	span := interval{seen.from, now}
	if !info.Since.IsZero() && info.Since.Before(span.from) {
		span.from = info.Since
	}
	if span.from.Before(from) {
		span.from = from
	}
	if info.Deleted {
		span.to = seen.to
		if !info.Until.IsZero() && info.Until.Before(now) {
			span.to = info.Until
		}
	}
	return span
}

// timelineStatsInput is what the stats are derived from: the deployments
// whose instances held cards in scope, every deployment's switches, each
// one's awake stretches and the span of its segments, and where the live
// instances are now.
type timelineStatsInput struct {
	deployments map[uint]store.ResidencyDeployment
	counts      map[uint]store.ResidencySwitches
	awake       map[uint][]interval
	seen        map[uint]interval
	placed      placements
	from, now   time.Time
}

// timelineDeployments are the stats of the deployments that share GPUs.
// AwakeShare is the awake time over the part of the window the deployment
// existed in (PresentSeconds), so one created or deleted inside the window is
// not understated.
func (a *apiHandlers) timelineDeployments(in timelineStatsInput) ([]dto.TimelineDeployment, error) {
	var shared []uint
	for id, info := range in.deployments {
		if listedInSharing(info, in.counts[id]) {
			shared = append(shared, id)
		}
	}
	out := []dto.TimelineDeployment{}
	if len(shared) == 0 {
		return out, nil
	}
	waits := a.holdWaits(shared, in.from, in.now)
	cards, err := a.cardsOf(in.placed, shared)
	if err != nil {
		return nil, err
	}
	for _, id := range shared {
		info, c, wait := in.deployments[id], in.counts[id], waits[id]
		seconds := unionSeconds(in.awake[id])
		present := presence(info, in.seen[id], in.from, in.now).seconds()
		share := 0.0
		if present > 0 {
			share = min(float64(seconds)/float64(present), 1)
		}
		out = append(out, dto.TimelineDeployment{
			DeploymentID: id, Deployment: info.Name, DeploymentDeleted: info.Deleted, YieldPolicy: info.Yield,
			AwakeSeconds: seconds, AwakeShare: share, PresentSeconds: present,
			Switches: c.Yielded + c.Wakes, Yielded: c.Yielded, YieldedTo: c.YieldedTo, Wakes: c.Wakes,
			HeldRequests: wait.held, WaitP50Ms: wait.p50, WaitP95Ms: wait.p95,
			Cards: nonNilCards(cards[id]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deployment != out[j].Deployment {
			return out[i].Deployment < out[j].Deployment
		}
		return out[i].DeploymentID < out[j].DeploymentID
	})
	return out, nil
}
