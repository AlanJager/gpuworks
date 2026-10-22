package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func timelineTestInstance(t *testing.T, st *Store, uuid string) *Instance {
	t.Helper()
	node, err := st.RegisterNode(uuid+"-node", HashToken(uuid+"-token"), uuid)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// writeFloodedTimeline reproduces the shape a pre-fold database has: long runs
// of rows that differ only in their byte counters, separated by the handful of
// rows that actually record a milestone.
func writeFloodedTimeline(t *testing.T, st *Store, inst *Instance, runs []struct {
	State, Phase string
	Rows         int
}) {
	t.Helper()
	for _, run := range runs {
		for i := 0; i < run.Rows; i++ {
			st.AddInstanceEventCode(inst, "state", instanceEventStatusUpdated, map[string]any{
				"state": run.State, "phase": run.Phase,
				"detail": fmt.Sprintf("%s %d", run.Phase, i), "bytes_done": i,
			})
		}
	}
}

func TestInstanceTimelineFoldsRunsAndReturnsNewestFirst(t *testing.T) {
	st := newTestStore(t)
	inst := timelineTestInstance(t, st, "fold-inst")
	writeFloodedTimeline(t, st, inst, []struct {
		State, Phase string
		Rows         int
	}{
		{proto.InstPulling, "image_pull", 500},
		{proto.InstPulling, "image_extract", 40},
		{proto.InstStarting, "container_start", 300},
		{proto.InstStarting, "health_check", 12},
		{proto.InstRunning, "", 1},
	})

	events, err := st.InstanceTimeline(inst.UUID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	// Five runs plus the INSTANCE_CREATED row the instance was born with.
	if len(events) != 6 {
		t.Fatalf("folded %d entries from 854 rows, want 6", len(events))
	}
	if events[0].ID <= events[len(events)-1].ID {
		t.Fatalf("timeline is not newest-first: %d then %d", events[0].ID, events[len(events)-1].ID)
	}
	// The surviving row of a run is its newest one, because that is the row
	// carrying the counters the phase finished on.
	var data map[string]any
	if err := json.Unmarshal([]byte(events[1].Data), &data); err != nil {
		t.Fatal(err)
	}
	if data["phase"] != "health_check" || data["detail"] != "health_check 11" {
		t.Fatalf("folded entry kept the wrong row of its run: %+v", data)
	}
}

// A run that straddles a page boundary must appear exactly once across the two
// pages: the fold cannot depend on where the cursor happens to fall.
func TestInstanceTimelinePaginatesWithoutDuplicatingOrLosingEntries(t *testing.T) {
	st := newTestStore(t)
	inst := timelineTestInstance(t, st, "page-inst")
	runs := make([]struct {
		State, Phase string
		Rows         int
	}, 0, 20)
	for i := 0; i < 20; i++ {
		runs = append(runs, struct {
			State, Phase string
			Rows         int
		}{proto.InstPulling, fmt.Sprintf("phase_%02d", i), 25})
	}
	writeFloodedTimeline(t, st, inst, runs)

	var all []InstanceEvent
	before := uint(0)
	for page := 0; page < 20; page++ {
		events, err := st.InstanceTimeline(inst.UUID, before, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			break
		}
		if len(events) > 3 {
			t.Fatalf("page %d returned %d entries, want at most the requested 3", page, len(events))
		}
		all = append(all, events...)
		before = events[len(events)-1].ID
	}
	// 20 folded runs plus the creation row.
	if len(all) != 21 {
		t.Fatalf("paged through %d entries, want 21", len(all))
	}
	seen := map[uint]bool{}
	previous := uint(0)
	for i, e := range all {
		if seen[e.ID] {
			t.Fatalf("entry %d (id %d) was returned twice across pages", i, e.ID)
		}
		seen[e.ID] = true
		if previous != 0 && e.ID >= previous {
			t.Fatalf("entry %d (id %d) is not older than the previous one (%d)", i, e.ID, previous)
		}
		previous = e.ID
	}
	whole, err := st.InstanceTimeline(inst.UUID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != len(all) {
		t.Fatalf("one page of 100 returned %d entries, paging returned %d", len(whole), len(all))
	}
	for i := range whole {
		if whole[i].ID != all[i].ID {
			t.Fatalf("entry %d differs between one page (%d) and paging (%d)", i, whole[i].ID, all[i].ID)
		}
	}
}

// Rows written before events carried codes hold free text in Data, and
// json_extract raises on malformed JSON. Such a row must neither break the
// query nor be folded into its neighbour just because its state and phase are
// both unreadable.
func TestInstanceTimelineKeepsPreCodeRowsDistinct(t *testing.T) {
	st := newTestStore(t)
	inst := timelineTestInstance(t, st, "legacy-inst")
	// The shape migration 87 leaves behind: identity backfilled onto the row,
	// Message and Data still free text.
	for _, legacy := range []struct{ message, data string }{
		{"scheduled on node-1", "not json at all"},
		{"scheduled on node-2", "also not json"},
		{"scheduled on node-2", "also not json"},
	} {
		if err := st.db.Create(&InstanceEvent{
			InstanceID: inst.ID, InstanceUUID: inst.UUID, DeploymentID: inst.DeploymentID,
			Kind: "state", Message: legacy.message, Data: legacy.data, CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	events, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("wrote %d rows, want 4", len(events))
	}
	folded, err := st.InstanceTimeline(inst.UUID, 0, 50)
	if err != nil {
		t.Fatalf("folding legacy rows failed: %v", err)
	}
	// The two identical node-2 rows fold together; the node-1 row and the
	// creation row stay on their own.
	if len(folded) != 3 {
		t.Fatalf("folded %d entries from 4 legacy rows, want 3: %+v", len(folded), folded)
	}
	if folded[0].Message != "scheduled on node-2" || folded[1].Message != "scheduled on node-1" {
		t.Fatalf("legacy rows folded into the wrong entries: %+v", folded)
	}
	if folded[2].Code != instanceEventCreated {
		t.Fatalf("oldest entry = %+v, want the creation row", folded[2])
	}
}

func TestEventTimelineLimitIsBounded(t *testing.T) {
	if got := NormalizeEventTimelineLimit(0); got != eventTimelineDefaultLimit {
		t.Fatalf("default limit = %d", got)
	}
	if got := NormalizeEventTimelineLimit(-5); got != eventTimelineDefaultLimit {
		t.Fatalf("negative limit = %d", got)
	}
	if got := NormalizeEventTimelineLimit(eventTimelineMaxLimit + 1000); got != eventTimelineMaxLimit {
		t.Fatalf("oversized limit = %d", got)
	}
	if got := NormalizeEventTimelineLimit(7); got != 7 {
		t.Fatalf("explicit limit = %d", got)
	}
}

func TestInstanceTimelineReadsAFloodedInstanceWithinBudget(t *testing.T) {
	st := newTestStore(t)
	inst := timelineTestInstance(t, st, "mirror-scale-inst")
	// A quarter of the worst single instance observed in production (76,838
	// rows). The point is the query plan, not the row count, so keep the test
	// affordable while still crossing every folding boundary many times.
	const rows = 20000
	runs := make([]struct {
		State, Phase string
		Rows         int
	}, 0, 20)
	for i := 0; i < 20; i++ {
		runs = append(runs, struct {
			State, Phase string
			Rows         int
		}{proto.InstPulling, fmt.Sprintf("phase_%02d", i), rows / 20})
	}
	writeFloodedTimeline(t, st, inst, runs)

	start := time.Now()
	events, err := st.InstanceTimeline(inst.UUID, 0, 50)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 21 {
		t.Fatalf("folded %d entries from %d rows, want 21", len(events), rows)
	}
	t.Logf("folded %d rows into %d entries in %s", rows, len(events), elapsed)
	if elapsed > 10*time.Second {
		t.Fatalf("folding %d rows took %s, which is not a usable page load", rows, elapsed)
	}
}
