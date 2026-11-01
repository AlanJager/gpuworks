package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// countingActivity counts the reads of the per-node switch counts.
type countingActivity struct {
	replayActivity
	onNodes atomic.Int64
}

func (c *countingActivity) DeploymentsOnNodes(nodes []string, from, to time.Time, want map[uint]bool) map[uint]store.ResidencySwitches {
	c.onNodes.Add(1)
	return c.replayActivity.DeploymentsOnNodes(nodes, from, to, want)
}

// rangeQuery is the query of a custom range between offsets from the rig's t0.
func (r *timelineRig) rangeQuery(since, until time.Duration) string {
	q := url.Values{"node": {r.node.UUID}, "since": {r.t0.Add(since).Format(time.RFC3339)}}
	if until != 0 {
		q.Set("until", r.t0.Add(until).Format(time.RFC3339))
	}
	return "?" + q.Encode()
}

func (r *timelineRig) read(query string) dto.SchedulingTimeline {
	r.t.Helper()
	status, body := r.get(query)
	var got dto.SchedulingTimeline
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		r.t.Fatalf("%s = %d %s", query, status, body)
	}
	return got
}

// seedRange gives the rig the history a custom range is read from: the
// shared instance is created, runs, sleeps (L2) at +1h and wakes at +2h, and
// yields at +10m, +1h and +2h45m; an instance is created on card 0 at
// +2h45m; another served card 1 until it was removed at +2h45m.
func (r *timelineRig) seedRange() {
	// Event ids grow with time, as the store reads assume: in order.
	ev := func(uuid string, dep uint, kind, code string, at time.Duration, data string) {
		r.event(uuid, dep, kind, code, at, data)
	}
	yield := func(at time.Duration) {
		ev("", r.shared.ID, store.EventKindAutomation, "AUTO_SLEEP", at, fmt.Sprintf(`{"trigger":"preempt","reason":"rank","for_deployment":%d,"level":1,"instances":["inst-shared"]}`, r.plain.ID))
	}
	ev("inst-gone", r.plain.ID, "state", "INSTANCE_STATUS_UPDATED", -time.Hour, `{"state":"running","phase":""}`)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_CREATED", 0, `{"state":"pending"}`)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 10*time.Minute, `{"state":"running","phase":""}`)
	yield(10 * time.Minute)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", time.Hour, `{"state":"running","phase":"sleep"}`)
	yield(time.Hour)
	ev("inst-shared", r.shared.ID, "sleep", "INSTANCE_SLEEP_RESULT", time.Hour+time.Minute, `{"result":"applied","level":2}`)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", time.Hour+time.Minute, `{"state":"running","phase":""}`)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 2*time.Hour, `{"state":"running","phase":"sleep"}`)
	ev("inst-shared", r.shared.ID, "sleep", "INSTANCE_SLEEP_RESULT", 2*time.Hour+time.Minute, `{"result":"applied","level":0}`)
	ev("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 2*time.Hour+time.Minute, `{"state":"running","phase":""}`)
	yield(2*time.Hour + 45*time.Minute)
	ev("inst-late", r.plain.ID, "state", "INSTANCE_CREATED", 2*time.Hour+45*time.Minute, `{"state":"pending"}`)
	ev("inst-gone", r.plain.ID, store.EventKindResidency, store.EventInstanceRemoved, 2*time.Hour+45*time.Minute, fmt.Sprintf(`{"node_id":%d,"gpus":[1]}`, r.node.ID))
	late := store.Instance{UUID: "inst-late", DeploymentID: r.plain.ID, NodeID: r.node.ID, State: "running", GPUIndexes: "[0]", CreatedAt: r.t0.Add(2*time.Hour + 45*time.Minute)}
	if err := r.st.DB().Create(&late).Error; err != nil {
		r.t.Fatal(err)
	}
}

// A custom range answers the lanes of that stretch: segments clipped to it,
// an instance created after it left out, one removed after it still serving
// to its end, the deployments' figures counted over it, and the answer names
// the range (custom, its key, from and to) on the wire.
func TestTimelineCustomRange(t *testing.T) {
	r := newTimelineRig(t)
	r.seedRange()
	// From +30m to +2h30m, which ended 30 minutes ago.
	status, body := r.get(r.rangeQuery(30*time.Minute, 2*time.Hour+30*time.Minute))
	if status != http.StatusOK {
		t.Fatalf("custom range = %d %s", status, body)
	}
	from, to := r.t0.Add(30*time.Minute).Format(time.RFC3339), r.t0.Add(2*time.Hour+30*time.Minute).Format(time.RFC3339)
	if want := fmt.Sprintf(`"window":{"from":%q,"to":%q,"hours":2,"key":"custom","seconds":7200,"custom":true},"windows":[{"key":"15m","seconds":900},{"key":"1h","seconds":3600},{"key":"6h","seconds":21600},{"key":"24h","seconds":86400}]`, from, to); !strings.Contains(body, want) {
		t.Fatalf("window\n got  %s\n want %s", body, want)
	}
	var got dto.SchedulingTimeline
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.SwitchesAvailable || got.Scope != dto.TimelineScopeNode {
		t.Fatalf("scope %q switches_available %v", got.Scope, got.SwitchesAvailable)
	}
	label := func(segs []dto.TimelineSegment) string {
		var parts []string
		for _, s := range segs {
			state := s.State
			if s.Level > 0 {
				state += fmt.Sprintf("-L%d", s.Level)
			}
			parts = append(parts, fmt.Sprintf("%s:%s[%d,%d]", s.Instance, state, int(s.From.Sub(r.t0).Minutes()), int(s.To.Sub(r.t0).Minutes())))
		}
		return strings.Join(parts, " ")
	}
	card0, card1 := got.Nodes[0].GPUs[0].Segments, got.Nodes[0].GPUs[1].Segments
	if want := "inst-shared:awake[30,60] inst-shared:switching[60,61] inst-shared:asleep-L2[61,120] inst-shared:switching[120,121] inst-shared:awake[121,150]"; label(card0) != want {
		t.Errorf("card 0 = %s\nwant   %s", label(card0), want)
	}
	// Card 1: the resident, the neighbour and the one removed after the range
	// still serving at its end; inst-late, created after it, is nowhere.
	if want := "inst-gone:awake[30,150] inst-plain:awake[30,150] inst-res:awake[30,150]"; label(card1) != want {
		t.Errorf("card 1 = %s\nwant   %s", label(card1), want)
	}
	if strings.Contains(body, "inst-late") {
		t.Errorf("an instance created after the range is in it: %s", body)
	}
	// The shared deployment: awake 30m + 29m of the 120m it held the card,
	// and the one yield inside the range - not the one before or after it.
	if len(got.Deployments) != 1 {
		t.Fatalf("deployments = %+v", got.Deployments)
	}
	d := got.Deployments[0]
	if d.AwakeSeconds != 59*60 || d.PresentSeconds != 120*60 || d.Yielded != 1 || d.Switches != 1 || d.Wakes != 0 {
		t.Errorf("figures = awake %d present %d yielded %d switches %d wakes %d, want 3540 7200 1 1 0", d.AwakeSeconds, d.PresentSeconds, d.Yielded, d.Switches, d.Wakes)
	}
	// An until left out is now: the range runs to the present.
	open := r.read(r.rangeQuery(30*time.Minute, 0))
	if open.Window.Key != "custom" || !open.Window.Custom || time.Since(open.Window.To) > time.Minute || open.Window.Seconds < 2*3600+29*60 {
		t.Errorf("open-ended range = %+v", open.Window)
	}
	if label(open.Nodes[0].GPUs[0].Segments) == "" || !strings.Contains(label(open.Nodes[0].GPUs[0].Segments), "inst-late:") {
		t.Errorf("a range to now lacks the instance created after the earlier range: %s", label(open.Nodes[0].GPUs[0].Segments))
	}
}

// The deployments' yields and wakes are in the scheduling activity's memory,
// the last 24 hours: a range that begins before that has no figures, says so
// and does not ask for them; one inside it does.
func TestTimelineCustomRangeBeyondTheActivityMemory(t *testing.T) {
	r := newTimelineRig(t)
	r.seedRange()
	act := &countingActivity{replayActivity: replayActivity{r.st}}
	a := &apiHandlers{d: Deps{Store: r.st, SchedulingActivity: act}}
	node := url.Values{"node": {r.node.UUID}}
	ask := func(extra url.Values) dto.SchedulingTimeline {
		t.Helper()
		q := url.Values{"node": node["node"]}
		for k, v := range extra {
			q[k] = v
		}
		rec := callHandler(a.schedulingTimeline, "/api/v1/scheduling/timeline?"+q.Encode())
		var out dto.SchedulingTimeline
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%v = %d %s", extra, rec.Code, rec.Body)
		}
		return out
	}
	now := time.Now().UTC()
	old := ask(url.Values{"since": {now.Add(-30 * time.Hour).Format(time.RFC3339)}, "until": {now.Add(-26 * time.Hour).Format(time.RFC3339)}})
	if old.SwitchesAvailable || act.onNodes.Load() != 0 {
		t.Fatalf("a range before the memory: switches_available %v, asked the activity %d times", old.SwitchesAvailable, act.onNodes.Load())
	}
	if len(old.Nodes[0].GPUs[1].Segments) != 1 || old.Nodes[0].GPUs[1].Segments[0].Instance != "inst-res" {
		t.Errorf("its lanes are still read: %+v", old.Nodes[0].GPUs[1].Segments)
	}
	recent := ask(url.Values{"since": {now.Add(-23 * time.Hour).Format(time.RFC3339)}})
	if !recent.SwitchesAvailable || act.onNodes.Load() != 1 {
		t.Fatalf("a range inside the memory: switches_available %v, asked %d times", recent.SwitchesAvailable, act.onNodes.Load())
	}
	// The quick windows are all inside it.
	for _, win := range []string{"15m", "1h", "6h", "24h"} {
		if got := ask(url.Values{"window": {win}}); !got.SwitchesAvailable || got.Window.Custom || got.Window.Key != win {
			t.Errorf("%s = %+v, switches_available %v", win, got.Window, got.SwitchesAvailable)
		}
	}
}

// Every quick window is a window ending now, listed by the answer: 15m, 1h,
// 6h (the default) and 24h.
func TestTimelineQuickWindows(t *testing.T) {
	r := newTimelineRig(t)
	for window, seconds := range map[string]int64{"15m": 900, "1h": 3600, "6h": 21600, "24h": 86400} {
		got := r.read("?node=" + r.node.UUID + "&window=" + window)
		if got.Window.Key != window || got.Window.Seconds != seconds || got.Window.Custom || got.Window.To.Sub(got.Window.From) != time.Duration(seconds)*time.Second {
			t.Errorf("%s = %+v", window, got.Window)
		}
		if len(got.Windows) != 4 || got.Windows[0].Key != "15m" || got.Windows[2].Key != "6h" {
			t.Errorf("%s lists %+v", window, got.Windows)
		}
	}
	if got := r.read("?node=" + r.node.UUID); got.Window.Key != "6h" {
		t.Errorf("default window = %q, want 6h", got.Window.Key)
	}
	// The older hours parameter still names the same windows.
	if got := r.read("?node=" + r.node.UUID + "&hours=24"); got.Window.Key != "24h" || got.Window.Hours != 24 {
		t.Errorf("hours=24 = %+v", got.Window)
	}
	// A 15 minute window holds the last 15 minutes of what the hour holds.
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 0, `{"state":"running","phase":""}`)
	quarter := r.read("?node=" + r.node.UUID + "&window=15m")
	seg := quarter.Nodes[0].GPUs[0].Segments
	if len(seg) != 1 || seg[0].To.Sub(seg[0].From) != 15*time.Minute {
		t.Errorf("15m lanes = %+v", seg)
	}
}

// Every malformed range is 400 FIELD_INVALID naming the parameter.
func TestTimelineCustomRangeRefusals(t *testing.T) {
	r := newTimelineRig(t)
	now := time.Now().UTC()
	at := func(d time.Duration) string { return url.QueryEscape(now.Add(d).Format(time.RFC3339)) }
	node := "?node=" + r.node.UUID + "&"
	for name, c := range map[string]struct{ query, field string }{
		"since not a time":          {node + "since=yesterday", "since"},
		"until not a time":          {node + "since=" + at(-time.Hour) + "&until=soon", "until"},
		"until without since":       {node + "until=" + at(-time.Hour), "since"},
		"since after until":         {node + "since=" + at(-time.Hour) + "&until=" + at(-2*time.Hour), "until"},
		"since is until":            {node + "since=" + at(-time.Hour) + "&until=" + at(-time.Hour), "until"},
		"until in the future":       {node + "since=" + at(-time.Hour) + "&until=" + at(time.Hour), "until"},
		"since in the future":       {node + "since=" + at(time.Hour), "until"},
		"more than seven days":      {node + "since=" + at(-8*24*time.Hour) + "&until=" + at(-time.Hour), "since"},
		"window with since":         {node + "window=6h&since=" + at(-time.Hour), "window"},
		"window with until":         {node + "window=6h&since=" + at(-time.Hour) + "&until=" + at(-time.Minute), "window"},
		"hours with since":          {node + "hours=6&since=" + at(-time.Hour), "hours"},
		"unknown window":            {node + "window=7d", "window"},
		"sharing with since":        {"?sharing=1&since=" + at(-time.Hour), "since"},
		"sharing with until":        {"?sharing=1&until=" + at(-time.Hour), "until"},
		"sharing with a long since": {"?sharing=1&window=1h&since=" + at(-time.Hour), "since"},
	} {
		status, body := r.get(c.query)
		if status != http.StatusBadRequest || !strings.Contains(body, `"code":"FIELD_INVALID"`) || !strings.Contains(body, `"field":"`+c.field+`"`) {
			t.Errorf("%s: %s = %d %s, want 400 FIELD_INVALID on %s", name, c.query, status, body, c.field)
		}
	}
	// Seven days exactly, and an until a few seconds ahead of the server's
	// clock, are fine; the latter ends at now.
	for name, query := range map[string]string{
		"seven days":      node + "since=" + at(-7*24*time.Hour-time.Minute) + "&until=" + at(-time.Minute),
		"a little ahead":  node + "since=" + at(-time.Hour) + "&until=" + at(20*time.Second),
		"since alone":     node + "since=" + at(-time.Hour),
		"fractional time": node + "since=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339Nano)),
	} {
		if status, body := r.get(query); status != http.StatusOK {
			t.Errorf("%s: %s = %d %s, want 200", name, query, status, body)
		}
	}
	ahead := r.read(node + "since=" + at(-time.Hour) + "&until=" + at(20*time.Second))
	if ahead.Window.To.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("an until ahead of now ends at %s, after now", ahead.Window.To)
	}
}

// The answer of a custom range in the past is read from the store once, per
// range and page of nodes, and kept a while; a window that ends now, or a
// range that ended a moment ago, is read every time; the cache is bounded.
func TestTimelinePastRangesAreKept(t *testing.T) {
	r := newTimelineRig(t)
	r.seedRange()
	a := &apiHandlers{d: Deps{Store: r.st, SchedulingActivity: replayActivity{r.st}}}
	now := time.Now().UTC().Truncate(time.Second)
	past := timelineRange{custom: true, from: r.t0.Add(30 * time.Minute), to: r.t0.Add(2*time.Hour + 30*time.Minute)}
	uuids := []string{r.node.UUID}
	first, err := a.cachedResidencyTimeline(uuids, past, now)
	if err != nil {
		t.Fatal(err)
	}
	// A change since (an instance that held card 0 from +1h) changes what a
	// fresh read says; the kept answer does not.
	since := store.Instance{UUID: "inst-since", DeploymentID: r.res.ID, NodeID: r.node.ID, State: "running", GPUIndexes: "[0]", CreatedAt: r.t0.Add(time.Hour)}
	if err := r.st.DB().Create(&since).Error; err != nil {
		t.Fatal(err)
	}
	fresh, err := a.residencyTimeline(uuids, past, now)
	if err != nil || timelineSegments(fresh) == timelineSegments(first) {
		t.Fatalf("the later event changes nothing in a fresh read, so the test proves nothing: %v", err)
	}
	for i := 0; i < 4; i++ {
		again, err := a.cachedResidencyTimeline(uuids, past, now)
		if err != nil || timelineSegments(again) != timelineSegments(first) {
			t.Fatalf("read %d: %v, %d segments, want the kept %d", i, err, timelineSegments(again), timelineSegments(first))
		}
	}
	if got := a.overview.timeline.reads.Load(); got != 1 {
		t.Fatalf("five requests for one past range read the store %d times, want 1", got)
	}
	// The same range for another page of nodes, or another range, is another read.
	if _, err := a.cachedResidencyTimeline([]string{r.node.UUID, "gone"}, past, now); err != nil {
		t.Fatal(err)
	}
	other := past
	other.to = other.to.Add(-time.Minute)
	if _, err := a.cachedResidencyTimeline(uuids, other, now); err != nil {
		t.Fatal(err)
	}
	if got := a.overview.timeline.reads.Load(); got != 3 {
		t.Fatalf("three distinct requests read %d times, want 3", got)
	}
	// An old entry is read again.
	key := past.from.Format(time.RFC3339) + "/" + past.to.Format(time.RFC3339) + "/" + r.node.UUID
	entry := a.overview.timeline.byKey[key]
	entry.at = entry.at.Add(-timelinePastTTL)
	a.overview.timeline.byKey[key] = entry
	if _, err := a.cachedResidencyTimeline(uuids, past, now); err != nil || a.overview.timeline.reads.Load() != 4 {
		t.Fatalf("an old entry was not read again: %v", err)
	}
	// Windows that end now, and ranges that ended a moment ago, are not kept.
	kept := len(a.overview.timeline.byKey)
	for _, rng := range []timelineRange{
		quickRange(nodeWindows[1], now),
		{custom: true, from: now.Add(-time.Hour), to: now.Add(-10 * time.Second)},
	} {
		for i := 0; i < 2; i++ {
			if _, err := a.cachedResidencyTimeline(uuids, rng, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(a.overview.timeline.byKey) != kept || a.overview.timeline.reads.Load() != 4 {
		t.Fatalf("a live range was kept: %d entries (was %d), %d reads", len(a.overview.timeline.byKey), kept, a.overview.timeline.reads.Load())
	}
	// The cache holds a bounded number of answers.
	for i := 0; i < 3*timelineCacheSize; i++ {
		rng := timelineRange{custom: true, from: past.from.Add(-time.Duration(i) * time.Second), to: past.to}
		if _, err := a.cachedResidencyTimeline(uuids, rng, now); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.overview.timeline.byKey); n > timelineCacheSize {
		t.Fatalf("%d answers kept, the bound is %d", n, timelineCacheSize)
	}
}
