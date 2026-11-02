package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// summarizeBetween derives every instance's segments over [since, until] and
// names them by instance: "state[from,to]" in hours from now.
func summarizeBetween(insts []ResidencyInstance, events map[string][]ResidencyEvent, since, until, now time.Time) map[string]string {
	out := map[string]string{}
	for _, inst := range insts {
		var parts []string
		for _, seg := range DeriveResidency(inst, events[inst.EventsOf()], since, until) {
			label := string(seg.State)
			if seg.Level > 0 {
				label += fmt.Sprintf("-L%d", seg.Level)
			}
			parts = append(parts, fmt.Sprintf("%s[%.1f,%.1f]", label, seg.From.Sub(now).Hours(), seg.To.Sub(now).Hours()))
		}
		out[inst.UUID] = strings.Join(parts, " ")
	}
	return out
}

// seedResidencyRange lays out the history around a past window [-10h, -4h]
// on one node:
//
//	through      live, asleep (L1) from -7.5h, woken at -3h (after the window)
//	straddler    live, asleep L2 since before the window, no event inside it
//	inside       live, created at -6h, serving from -5.5h
//	late         live, created at -2h (after the window)
//	left-inside  removed at -6h, serving before the window
//	left-after   removed at -2h, serving before the window
//	left-before  removed at -11h (before the window)
func seedResidencyRange(t *testing.T) (s *Store, node *Node, now time.Time, rel func(float64) time.Time) {
	t.Helper()
	s, node, ids := seedResidency(t)
	now = time.Now().UTC().Truncate(time.Second)
	rel = func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	dep := ids["alpha"]
	for _, in := range []Instance{
		{UUID: "through", DeploymentID: dep, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: rel(-20)},
		{UUID: "straddler", DeploymentID: dep, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: 2, CreatedAt: rel(-30)},
		{UUID: "inside", DeploymentID: dep, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", CreatedAt: rel(-6)},
		{UUID: "late", DeploymentID: dep, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: rel(-2)},
	} {
		in := in
		if err := s.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	running := `{"state":"running","phase":""}`
	removed := fmt.Sprintf(`{"node_id":%d,"gpus":[0]}`, node.ID)
	putEvent(t, s, "through", dep, "state", instanceEventStatusUpdated, rel(-19), running)
	putEvent(t, s, "straddler", dep, "state", instanceEventStatusUpdated, rel(-25), running)
	putEvent(t, s, "straddler", dep, "sleep", instanceEventSleepResult, rel(-20), `{"result":"applied","level":2}`)
	putEvent(t, s, "left-before", dep, "state", instanceEventStatusUpdated, rel(-15), running)
	putEvent(t, s, "left-inside", dep, "state", instanceEventStatusUpdated, rel(-12), running)
	putEvent(t, s, "left-after", dep, "state", instanceEventStatusUpdated, rel(-12), running)
	putEvent(t, s, "left-before", dep, EventKindResidency, EventInstanceRemoved, rel(-11), removed)
	// The window: from -10h to -4h.
	putEvent(t, s, "through", dep, "state", instanceEventStatusUpdated, rel(-8), `{"state":"running","phase":"sleep"}`)
	putEvent(t, s, "through", dep, "sleep", instanceEventSleepResult, rel(-7.5), `{"result":"applied","level":1}`)
	putEvent(t, s, "through", dep, "state", instanceEventStatusUpdated, rel(-7.5), running)
	putEvent(t, s, "inside", dep, "state", instanceEventCreated, rel(-6), `{"state":"pending"}`)
	putEvent(t, s, "left-inside", dep, EventKindResidency, EventInstanceRemoved, rel(-6), removed)
	putEvent(t, s, "inside", dep, "state", instanceEventStatusUpdated, rel(-5.5), running)
	// After the window.
	putEvent(t, s, "through", dep, "state", instanceEventStatusUpdated, rel(-3), `{"state":"running","phase":"sleep"}`)
	putEvent(t, s, "through", dep, "sleep", instanceEventSleepResult, rel(-2.9), `{"result":"applied","level":0}`)
	putEvent(t, s, "through", dep, "state", instanceEventStatusUpdated, rel(-2.9), running)
	putEvent(t, s, "left-after", dep, EventKindResidency, EventInstanceRemoved, rel(-2), removed)
	putEvent(t, s, "late", dep, "state", instanceEventCreated, rel(-2), `{"state":"pending"}`)
	return s, node, now, rel
}

// A window that ended in the past holds the instances that held cards then
// and what they did until it ended, and nothing from after: one created later
// is out, one removed later is still there at the end, one removed inside it
// ends where it was removed, and one that was asleep when it began is asleep
// from its start.
func TestResidencyInputsBetweenBoundsTheWindow(t *testing.T) {
	s, node, now, rel := seedResidencyRange(t)
	since, until := rel(-10), rel(-4)
	insts, events, err := s.ResidencyInputsBetween([]uint{node.ID}, since, until)
	if err != nil {
		t.Fatal(err)
	}
	got := summarizeBetween(insts, events, since, until, now)
	want := map[string]string{
		"through":     "awake[-10.0,-8.0] switching[-8.0,-7.5] asleep-L1[-7.5,-4.0]",
		"straddler":   "asleep-L2[-10.0,-4.0]",
		"inside":      "starting[-6.0,-5.5] awake[-5.5,-4.0]",
		"left-inside": "awake[-10.0,-6.0]",
		"left-after":  "awake[-10.0,-4.0]",
	}
	if len(got) != len(want) {
		t.Fatalf("instances = %v, want %v", got, want)
	}
	for uuid, segs := range want {
		if got[uuid] != segs {
			t.Errorf("%s = %q, want %q", uuid, got[uuid], segs)
		}
	}
	// The reads themselves stop where the window ends: no event after it was
	// read, which the derivation would otherwise have to skip.
	for uuid, list := range events {
		for _, ev := range list {
			if ev.At.After(until) {
				t.Errorf("%s: read an event at %s, after the window ended at %s", uuid, ev.At, until)
			}
		}
	}
	// The newest state and level before the window, then the three inside it.
	if n := len(events["through"]); n != 4 {
		t.Errorf("through has %d events, want 4: %+v", n, events["through"])
	}
	// left-after's removal is after the window: it was still serving.
	for _, ev := range events["left-after"] {
		if ev.Code == EventInstanceRemoved {
			t.Errorf("left-after's removal at %s was read for a window that ended before it", ev.At)
		}
	}
}

// With no end the window runs to now, as ResidencyInputs reads it.
func TestResidencyInputsBetweenWithoutAnEndRunsToNow(t *testing.T) {
	s, node, now, rel := seedResidencyRange(t)
	since := rel(-10)
	a, ea, err := s.ResidencyInputs([]uint{node.ID}, since)
	if err != nil {
		t.Fatal(err)
	}
	b, eb, err := s.ResidencyInputsBetween([]uint{node.ID}, since, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	gotA, gotB := summarizeBetween(a, ea, since, now, now), summarizeBetween(b, eb, since, now, now)
	if fmt.Sprint(gotA) != fmt.Sprint(gotB) {
		t.Fatalf("ResidencyInputs and an open end differ:\n%v\n%v", gotA, gotB)
	}
	if want := "awake[-10.0,-8.0] switching[-8.0,-7.5] asleep-L1[-7.5,-3.0] switching[-3.0,-2.9] awake[-2.9,0.0]"; gotB["through"] != want {
		t.Errorf("through to now = %q, want %q", gotB["through"], want)
	}
	if gotB["left-after"] != "awake[-10.0,-2.0]" || gotB["late"] != "starting[-2.0,0.0]" {
		t.Errorf("to now: left-after %q, late %q", gotB["left-after"], gotB["late"])
	}
}

// A window with no event inside it still knows each instance's state from the
// events before it; an instance whose first event is after it is out.
func TestResidencyInputsBetweenAQuietWindow(t *testing.T) {
	s, node, now, rel := seedResidencyRange(t)
	since, until := rel(-18), rel(-17) // nothing happens between -18h and -17h
	insts, events, err := s.ResidencyInputsBetween([]uint{node.ID}, since, until)
	if err != nil {
		t.Fatal(err)
	}
	got := summarizeBetween(insts, events, since, until, now)
	want := map[string]string{
		"through":     "awake[-18.0,-17.0]",     // created at -20h, serving from -19h
		"straddler":   "asleep-L2[-18.0,-17.0]", // asleep since -20h
		"left-inside": "",                       // first event at -12h: nothing yet
		"left-after":  "",
		"left-before": "",
	}
	for uuid, segs := range want {
		if got[uuid] != segs {
			t.Errorf("%s = %q, want %q (all: %v)", uuid, got[uuid], segs, got)
		}
	}
	for _, out := range []string{"inside", "late"} {
		if _, ok := got[out]; ok {
			t.Errorf("%s, created after the window, is in it", out)
		}
	}
}

// The reads that bound a window stay ranges of an index: the bounded events
// by (instance_uuid, id), the removals by (kind, id) and the edges by
// created_at, none sorting or scanning the table.
func TestResidencyBoundedQueryPlans(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	cases := []struct {
		name string
		q    *gorm.DB
		uses string
	}{
		{"bounded events", residencyWindowQuery(db, []string{"a", "b"}, 100, 200), "idx_instance_events_uuid_id"},
		{"removed", residencyRemovedQuery(db, 100), "idx_instance_events_kind_id"},
		{"window end", residencyBoundaryQuery(db, residencyBase), "created_at"},
	}
	for _, c := range cases {
		stmt := c.q.Session(&gorm.Session{DryRun: true}).Find(&[]InstanceEvent{}).Statement
		var plan []struct{ Detail string }
		if err := db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var text []string
		for _, row := range plan {
			text = append(text, row.Detail)
		}
		joined := strings.Join(text, " | ")
		t.Logf("%s: %s", c.name, joined)
		if !strings.Contains(joined, c.uses) || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s plan = %s, want a range of an index on %s and no sort", c.name, joined, c.uses)
		}
	}
}
