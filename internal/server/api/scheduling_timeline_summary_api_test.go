package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// seedSwaps makes card 0 change hands every 30 seconds for the first two
// hours after the rig's t0: the shared deployment's instance and another
// deployment's take turns being the one running (the other has failed), 240
// stretches in all, the last of which runs on to now. It returns the other
// deployment.
func (r *timelineRig) seedSwaps() *store.Deployment {
	other := r.res
	b := store.Instance{UUID: "inst-b", DeploymentID: other.ID, NodeID: r.node.ID, State: "running", GPUIndexes: "[0]", CreatedAt: r.t0}
	if err := r.st.DB().Create(&b).Error; err != nil {
		r.t.Fatal(err)
	}
	for i := 0; i < 240; i++ {
		at := time.Duration(i) * 30 * time.Second
		a, bb := "running", "failed"
		if i%2 == 1 {
			a, bb = bb, a
		}
		r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", at, fmt.Sprintf(`{"state":%q,"phase":""}`, a))
		r.event("inst-b", other.ID, "state", "INSTANCE_STATUS_UPDATED", at, fmt.Sprintf(`{"state":%q,"phase":""}`, bb))
	}
	return other
}

// A card that changed hands more often than the cap lists the newest 200
// segments raw and the time before them as summaries, on the wire: exact
// keys and values, the boundary named, and a card that did not overflow
// keeps the shape it always had.
func TestSchedulingTimelineSummaryWire(t *testing.T) {
	r := newTimelineRig(t)
	other := r.seedSwaps()
	a := &apiHandlers{d: Deps{Store: r.st, SchedulingActivity: replayActivity{r.st}}}
	now := r.t0.Add(3 * time.Hour)
	out, err := a.residencyTimeline([]string{r.node.UUID}, quickRange(nodeWindows[2], now), now)
	if err != nil {
		t.Fatal(err)
	}
	gpus := out.Nodes[0].GPUs
	body, err := json.Marshal(gpus[0])
	if err != nil {
		t.Fatal(err)
	}
	// 240 stretches, the 200 newest raw; the first of those begins 20
	// minutes after t0. The 6 h window begins 3 h before t0, so the 3 h 20 m
	// before the boundary are 50 slices of 4 minutes, of which only the last
	// five hold the card: the 8 stretches of 30 s in each (4 minutes, two
	// deployments of 2 minutes each) change hands 7 times in the first, where
	// the first holder had none to take over from, and 8 in each other.
	f := func(d time.Duration) string { return r.t0.Add(d).Format(time.RFC3339) }
	lowID, lowName, highID, highName := r.shared.ID, "night-batch", other.ID, "always-on"
	if lowID > highID {
		lowID, lowName, highID, highName = highID, highName, lowID, lowName
	}
	var slices []string
	for k := 0; k < 5; k++ {
		switches := 8
		if k == 0 {
			switches = 7
		}
		slices = append(slices, fmt.Sprintf(`{"kind":"summary","from":%q,"to":%q,"switches":%d,"dominant_deployment_id":%d,"dominant_deployment":%q,"deployments":[`+
			`{"deployment_id":%d,"deployment":%q,"awake_seconds":120,"asleep_seconds":0,"switching_seconds":0,"starting_seconds":0,"state":"awake"},`+
			`{"deployment_id":%d,"deployment":%q,"awake_seconds":120,"asleep_seconds":0,"switching_seconds":0,"starting_seconds":0,"state":"awake"}]}`,
			f(time.Duration(k)*4*time.Minute), f(time.Duration(k+1)*4*time.Minute), switches, lowID, lowName, lowID, lowName, highID, highName))
	}
	want := fmt.Sprintf(`"truncated":true,"summarized_before":%q,"summary":[%s]}`, f(20*time.Minute), strings.Join(slices, ","))
	if !strings.HasSuffix(string(body), want) {
		t.Fatalf("card 0\n got  ...%s\n want ...%s", string(body)[max(len(body)-len(want)-40, 0):], want)
	}
	if len(gpus[0].Segments) != timelineSegmentCap || !gpus[0].Segments[0].From.Equal(r.t0.Add(20*time.Minute)) {
		t.Fatalf("%d raw segments from %v, want %d from +20m", len(gpus[0].Segments), gpus[0].Segments[0].From, timelineSegmentCap)
	}
	// A card that did not overflow has none of the new keys.
	plain, err := json.Marshal(gpus[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "summar") || !strings.Contains(string(plain), `"truncated":false}`) {
		t.Fatalf("card 1 = %s", plain)
	}
}

// Over HTTP: the card's keys are the old ones plus summarized_before and
// summary; a client that reads only the old keys gets the newest raw
// segments and truncated true, as before.
func TestSchedulingTimelineSummaryKeepsTheOldShape(t *testing.T) {
	r := newTimelineRig(t)
	r.seedSwaps()
	status, body := r.get("?node=" + r.node.UUID + "&window=6h")
	if status != http.StatusOK {
		t.Fatalf("timeline = %d %s", status, body)
	}
	var generic struct {
		Nodes []struct {
			GPUs []map[string]json.RawMessage `json:"gpus"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		t.Fatal(err)
	}
	keys := func(raw map[string]json.RawMessage) string {
		var ks []string
		for k := range raw {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if got := keys(generic.Nodes[0].GPUs[0]); got != "index,segments,summarized_before,summary,truncated" {
		t.Fatalf("card 0 keys = %s", got)
	}
	if got := keys(generic.Nodes[0].GPUs[1]); got != "index,segments,truncated" {
		t.Fatalf("card 1 keys = %s", got)
	}
	var summary []map[string]json.RawMessage
	if err := json.Unmarshal(generic.Nodes[0].GPUs[0]["summary"], &summary); err != nil || len(summary) == 0 || len(summary) > timelineSummaryBuckets {
		t.Fatalf("summary = %s (%v)", generic.Nodes[0].GPUs[0]["summary"], err)
	}
	// The last slice, next to the boundary, is full whatever second the request came in; the first may be a sliver.
	if got := keys(summary[len(summary)-1]); got != "deployments,dominant_deployment,dominant_deployment_id,from,kind,switches,to" {
		t.Fatalf("summary keys = %s", got)
	}
	var deps []map[string]json.RawMessage
	if err := json.Unmarshal(summary[len(summary)-1]["deployments"], &deps); err != nil || len(deps) != 2 {
		t.Fatalf("summary deployments = %s (%v)", summary[len(summary)-1]["deployments"], err)
	}
	if got := keys(deps[0]); got != "asleep_seconds,awake_seconds,deployment,deployment_id,starting_seconds,state,switching_seconds" {
		t.Fatalf("summary deployment keys = %s", got)
	}
	if string(summary[len(summary)-1]["kind"]) != `"summary"` {
		t.Fatalf("kind = %s", summary[len(summary)-1]["kind"])
	}
	// What a client from before summaries reads.
	var old struct {
		Nodes []struct {
			GPUs []struct {
				Index     int               `json:"index"`
				Segments  []json.RawMessage `json:"segments"`
				Truncated bool              `json:"truncated"`
			} `json:"gpus"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &old); err != nil {
		t.Fatal(err)
	}
	if card := old.Nodes[0].GPUs[0]; !card.Truncated || len(card.Segments) != timelineSegmentCap {
		t.Fatalf("an old client reads truncated %v with %d segments, want true with %d", card.Truncated, len(card.Segments), timelineSegmentCap)
	}
	// The summaries end where the raw segments begin.
	var first struct {
		From time.Time `json:"from"`
	}
	var segs []json.RawMessage
	_ = json.Unmarshal(generic.Nodes[0].GPUs[0]["segments"], &segs)
	_ = json.Unmarshal(segs[0], &first)
	if want := `"` + first.From.Format(time.RFC3339) + `"`; string(generic.Nodes[0].GPUs[0]["summarized_before"]) != want {
		t.Fatalf("summarized_before = %s, want the first raw segment's start %s", generic.Nodes[0].GPUs[0]["summarized_before"], want)
	}
}

func newCacheRig(t *testing.T) (*timelineRig, *apiHandlers, []string, time.Time) {
	t.Helper()
	r := newTimelineRig(t)
	a := &apiHandlers{d: Deps{Store: r.st, SchedulingActivity: replayActivity{r.st}}}
	return r, a, []string{r.node.UUID}, time.Now().UTC().Truncate(time.Second)
}

// A quick window's answer is read from the store once per window and page of
// nodes within timelineQuickTTL, and again after it.
func TestTimelineQuickWindowsAreKeptBriefly(t *testing.T) {
	r, a, uuids, now := newCacheRig(t)
	win := quickRange(nodeWindows[1], now)
	first, err := a.cachedResidencyTimeline(uuids, win, now)
	if err != nil {
		t.Fatal(err)
	}
	// Something lands on card 0 after the first read.
	late := store.Instance{UUID: "inst-late", DeploymentID: r.res.ID, NodeID: r.node.ID, State: "running", GPUIndexes: "[0]", CreatedAt: now.Add(-30 * time.Minute)}
	if err := r.st.DB().Create(&late).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := a.cachedResidencyTimeline(uuids, win, now.Add(time.Duration(i)*time.Second))
		if err != nil || timelineSegments(again) != timelineSegments(first) {
			t.Fatalf("request %d within the TTL: %v, %d segments, want the kept %d", i, err, timelineSegments(again), timelineSegments(first))
		}
	}
	if got := a.overview.timeline.reads.Load(); got != 1 {
		t.Fatalf("four requests within the TTL read the store %d times, want 1", got)
	}
	// Another window, or another page of nodes, is another read.
	if _, err := a.cachedResidencyTimeline(uuids, quickRange(nodeWindows[0], now), now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cachedResidencyTimeline([]string{r.node.UUID, "gone"}, win, now); err != nil {
		t.Fatal(err)
	}
	if got := a.overview.timeline.reads.Load(); got != 3 {
		t.Fatalf("three distinct requests read %d times, want 3", got)
	}
	// After the TTL it is read again, and sees what landed.
	key := win.cacheKey(uuids)
	entry := a.overview.timeline.byKey[key]
	if entry.ttl != timelineQuickTTL {
		t.Fatalf("a quick window is kept for %v, want %v", entry.ttl, timelineQuickTTL)
	}
	entry.at = entry.at.Add(-timelineQuickTTL)
	a.overview.timeline.byKey[key] = entry
	after, err := a.cachedResidencyTimeline(uuids, win, now)
	if err != nil || a.overview.timeline.reads.Load() != 4 || timelineSegments(after) == timelineSegments(first) {
		t.Fatalf("after the TTL: %v, %d reads, %d segments (was %d)", err, a.overview.timeline.reads.Load(), timelineSegments(after), timelineSegments(first))
	}
}

// Over HTTP the route answers a second request for the same window from the
// kept answer, byte for byte, though the store has changed.
func TestSchedulingTimelineRouteSharesQuickWindowReads(t *testing.T) {
	r := newTimelineRig(t)
	query := "?node=" + r.node.UUID + "&window=1h"
	_, first := r.get(query)
	late := store.Instance{UUID: "inst-late", DeploymentID: r.res.ID, NodeID: r.node.ID, State: "running", GPUIndexes: "[0]", CreatedAt: time.Now().UTC().Add(-30 * time.Minute)}
	if err := r.st.DB().Create(&late).Error; err != nil {
		t.Fatal(err)
	}
	if _, second := r.get(query); second != first {
		t.Fatalf("the second request within the TTL\n got  %s\n want %s", second, first)
	}
	// Another window sees the change.
	_, other := r.get("?node=" + r.node.UUID + "&window=15m")
	if !strings.Contains(other, "inst-late") {
		t.Fatalf("a read of another window does not see the instance: %s", other)
	}
}

// Askers of one key while it is being read share that read.
func TestTimelineConcurrentAskersShareOneRead(t *testing.T) {
	_, a, uuids, now := newCacheRig(t)
	win := quickRange(nodeWindows[2], now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.cachedResidencyTimeline(uuids, win, now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := a.overview.timeline.reads.Load(); got != 1 {
		t.Fatalf("eight concurrent askers read the store %d times, want 1", got)
	}
}

// A full cache drops the expired answers first and then the oldest; it never
// grows past its bound.
func TestTimelineCacheEvictsExpiredThenOldest(t *testing.T) {
	var c timelineCache
	now := time.Now().UTC()
	for i := 0; i < timelineCacheSize; i++ {
		c.keep(fmt.Sprint("k", i), timelineEntry{at: now.Add(-time.Duration(i) * time.Second), ttl: time.Hour})
	}
	// One expired entry in the middle goes before any live one.
	c.byKey["k5"] = timelineEntry{at: now.Add(-2 * time.Hour), ttl: time.Minute}
	c.keep("new", timelineEntry{at: now, ttl: time.Hour})
	if _, ok := c.byKey["k5"]; ok || len(c.byKey) != timelineCacheSize {
		t.Fatalf("expired entry kept: %v, %d entries", ok, len(c.byKey))
	}
	if _, ok := c.byKey[fmt.Sprint("k", timelineCacheSize-1)]; !ok {
		t.Fatal("a live entry was dropped while an expired one was there to go")
	}
	// Nothing expired: the oldest goes.
	c.keep("newer", timelineEntry{at: now, ttl: time.Hour})
	if _, ok := c.byKey[fmt.Sprint("k", timelineCacheSize-1)]; ok || len(c.byKey) != timelineCacheSize {
		t.Fatalf("the oldest was kept: %v, %d entries", ok, len(c.byKey))
	}
	// Replacing a key does not evict.
	c.keep("newer", timelineEntry{at: now, ttl: time.Hour})
	if len(c.byKey) != timelineCacheSize {
		t.Fatalf("%d entries after a replace", len(c.byKey))
	}
}

// An answer is a few elements for an overflowing card, and the cache counts
// summaries with the segments.
func TestTimelineSegmentsCountsSummaries(t *testing.T) {
	out := dto.SchedulingTimeline{Nodes: []dto.TimelineNode{{GPUs: []dto.TimelineGPU{{Segments: make([]dto.TimelineSegment, 3), Summary: make([]dto.TimelineSummary, 2)}}}}}
	if got := timelineSegments(out); got != 5 {
		t.Fatalf("elements = %d, want 5", got)
	}
}
