package schedactivity

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

type rig struct {
	st    *store.Store
	stats *statsdb.Store
	svc   *Service
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	stats, err := statsdb.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stats.Close() })
	r := &rig{st: st, stats: stats, now: time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)}
	retention := store.DefaultDataRetention(72*time.Hour, "UTC")
	r.svc = &Service{Tracker: NewTracker(), Store: st, Stats: stats, Retention: func() store.DataRetention { return retention }, Now: func() time.Time { return r.now }}
	r.svc.Tracker.Ready(r.now.Add(-replayReach).UnixMilli(), 0)
	return r
}

// Each quick window is read from the memory alone in its own bucket; a week
// reads hours from stats.db and the memory's last day; a range past the hour
// rows' retention reads days; one past the day rows' is flagged.
func TestPlanRangePicksBucketsAndSources(t *testing.T) {
	r := newRig(t)
	c := r.svc.coverage(r.now)
	for _, tc := range []struct {
		span, step time.Duration
		wantStep   time.Duration
		fromMemory bool
	}{
		{15 * time.Minute, time.Minute, time.Minute, true},
		{time.Hour, time.Minute, time.Minute, true},
		{6 * time.Hour, 5 * time.Minute, 5 * time.Minute, true},
		{24 * time.Hour, 15 * time.Minute, 15 * time.Minute, true},
		{7 * 24 * time.Hour, time.Hour, time.Hour, false},
		{3 * 24 * time.Hour, 0, time.Hour, false},
		{90 * time.Minute, 0, time.Minute, true},
		{20 * time.Hour, 0, 15 * time.Minute, true},
	} {
		p := r.svc.planRange(r.now.Add(-tc.span), r.now, tc.step, c)
		if time.Duration(p.step)*time.Millisecond != tc.wantStep || (p.memFrom == p.align) != tc.fromMemory || p.n > MaxBuckets+1 || p.beyond {
			t.Errorf("%v/%v: step %v, memory from %v align %v, %d buckets", tc.span, tc.step, time.Duration(p.step)*time.Millisecond, time.UnixMilli(p.memFrom), time.UnixMilli(p.align), p.n)
		}
		if !tc.fromMemory && (p.hoursFrom != p.align || p.memFrom%hourMS != 0) {
			t.Errorf("%v: hours from %v, memory from %v", tc.span, time.UnixMilli(p.hoursFrom), time.UnixMilli(p.memFrom))
		}
	}
	// 60 days: past the 31 days of hours, so days, aligned to a day.
	p := r.svc.planRange(r.now.Add(-60*24*time.Hour), r.now, 0, c)
	if p.step != 24*hourMS || p.daysFrom != p.align || p.hoursFrom <= p.align || p.hoursFrom%(24*hourMS) != 0 || p.beyond {
		t.Fatalf("60 days = %+v", p)
	}
	if p := r.svc.planRange(r.now.Add(-500*24*time.Hour), r.now, 0, c); !p.beyond {
		t.Fatal("a range past the day rows' retention is not flagged")
	}
}

// An answer adds stats.db's hours before the memory to the memory's minutes,
// stacks them by reason and names the cards that changed hands most.
func TestActivityAddsStoredHoursToTheMemory(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	node, err := r.st.RegisterNode("busy", store.HashToken("t"), "busy")
	if err != nil {
		t.Fatal(err)
	}
	old := r.now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
	if err := r.stats.ReplaceSchedulingHours(ctx, []statsdb.SchedulingHour{{
		HourMS: old.UnixMilli(), Reasons: map[string]int64{"idle_yield": 4, "queued": 2},
		Cards: []statsdb.SchedulingCard{{NodeUUID: node.UUID, GPU: 3, Switches: 5}, {NodeUUID: "gone-node", GPU: 0, Switches: 1}},
	}}); err != nil {
		t.Fatal(err)
	}
	r.svc.Tracker.Observe(yield(1, 1, 2, r.now.Add(-10*time.Minute), store.ActivityRankPreemption, store.EventCard{NodeUUID: node.UUID, GPU: 3}, store.EventCard{NodeUUID: node.UUID, GPU: 1}))
	week, err := r.svc.Activity(ctx, r.now.Add(-7*24*time.Hour), r.now, time.Hour, 20)
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]int64{}
	for _, reason := range week.Reasons {
		totals[reason.Key] = reason.Count
	}
	if week.BucketSeconds != 3600 || totals["idle_yield"] != 4 || totals["queued"] != 2 || totals["rank_preemption"] != 1 || week.BeyondRetention {
		t.Fatalf("week = %+v", week.Reasons)
	}
	var sum int64
	for _, b := range week.Buckets {
		if len(b.Counts) != len(week.Reasons) {
			t.Fatalf("bucket %+v", b)
		}
		for _, n := range b.Counts {
			sum += n
		}
		if b.Start.Equal(old) && fmt.Sprint(b.Counts) != "[4 0 0 0 0 0 2]" {
			t.Fatalf("stored hour bucket = %+v", b)
		}
	}
	if sum != 7 || !week.From.Equal(week.Buckets[0].Start) {
		t.Fatalf("buckets sum %d from %v", sum, week.From)
	}
	if fmt.Sprintf("%+v", week.TopCards) != fmt.Sprintf("[{NodeUUID:%s Node:busy GPUIndex:3 Switches:6} {NodeUUID:%s Node:busy GPUIndex:1 Switches:1} {NodeUUID:gone-node Node: GPUIndex:0 Switches:1}]", node.UUID, node.UUID) {
		t.Fatalf("top cards = %+v", week.TopCards)
	}
	if top, _ := r.svc.Activity(ctx, r.now.Add(-7*24*time.Hour), r.now, time.Hour, 1); len(top.TopCards) != 1 {
		t.Fatalf("top 1 = %+v", top.TopCards)
	}
	// The last hour is the memory's alone.
	hour, err := r.svc.Activity(ctx, r.now.Add(-time.Hour), r.now, time.Minute, 20)
	if err != nil || hour.Reasons[0].Count != 0 || hour.Reasons[1].Count != 1 || len(hour.Buckets) != 60 {
		t.Fatalf("hour = %+v %v", hour, err)
	}
}

// A flush writes every finished hour the memory holds whole, once; the last,
// when the server stops, the hour in progress too. A deployment's hour row is
// of one that shares cards, with its awake and present time.
func TestFlushWritesFinishedHoursOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	dep, err := r.st.CreateDeployment(yieldDeployment("shared", yieldpolicy.TimeSlice))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("created_at", r.now.Add(-48*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	hourStart := r.now.Truncate(time.Hour).Add(-time.Hour) // 11:00
	r.svc.Tracker.Observe(status(1, "s-1", dep.ID, hourStart.Add(-time.Hour), "running", ""))
	r.svc.Tracker.Observe(yield(2, dep.ID, 99, hourStart.Add(20*time.Minute), store.ActivityTimesliceTurn, store.EventCard{NodeUUID: "n", GPU: 0}))
	r.svc.Tracker.Observe(status(3, "s-1", dep.ID, hourStart.Add(20*time.Minute), "running", "sleep"))
	if err := r.svc.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	hourly, _, err := r.stats.RowCount(ctx, statsdb.KindScheduling)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.stats.SchedulingDeployments(ctx, statsdb.Range{From: hourStart.UnixMilli(), To: hourStart.Add(time.Hour).UnixMilli()})
	if err != nil || len(rows) != 1 || rows[0] != (statsdb.SchedulingDeployment{DeploymentID: dep.ID, AwakeSeconds: 1200, PresentSeconds: 3600, Yielded: 1}) {
		t.Fatalf("deployment hour = %+v %v", rows, err)
	}
	// The hour before: awake throughout, no event.
	before, _ := r.stats.SchedulingDeployments(ctx, statsdb.Range{From: hourStart.Add(-time.Hour).UnixMilli(), To: hourStart.UnixMilli()})
	if len(before) != 1 || before[0].AwakeSeconds != 3600 {
		t.Fatalf("hour before = %+v", before)
	}
	// Again within the hour: nothing new is written.
	r.svc.Tracker.Observe(yield(4, dep.ID, 99, r.now.Add(-time.Minute), store.ActivityIdleYield))
	if err := r.svc.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := r.stats.RowCount(ctx, statsdb.KindScheduling); again != hourly {
		t.Fatalf("a second flush in the hour wrote %d rows, had %d", again, hourly)
	}
	// The last flush writes the hour in progress.
	if err := r.svc.Flush(ctx, true); err != nil {
		t.Fatal(err)
	}
	current, _ := r.stats.SchedulingReasons(ctx, statsdb.Range{From: r.now.Truncate(time.Hour).UnixMilli(), To: r.now.UnixMilli()})
	if len(current) != 1 || current[0].Reason != "idle_yield" || current[0].Events != 1 {
		t.Fatalf("hour in progress = %+v", current)
	}
}

// The replay fills the memory from the event log: the scheduling events'
// reasons and cards, and the residency of the deployments that share cards.
func TestReplayFillsTheMemory(t *testing.T) {
	r := newRig(t)
	r.svc.Tracker = NewTracker()
	r.now = time.Now().UTC()
	node, err := r.st.RegisterNode("rn", store.HashToken("t"), "rn")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := r.st.CreateDeployment(yieldDeployment("replayed", yieldpolicy.Anytime))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateInstance(&store.Instance{UUID: "r-1", DeploymentID: dep.ID, NodeID: node.ID, GPUIndexes: "[2]"}); err != nil {
		t.Fatal(err)
	}
	r.st.AddDeploymentEventCode(dep.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "idle", "for_deployment": 42, "instances": []string{"r-1"}})
	read, dropped, err := r.svc.Replay(r.now)
	if err != nil || read == 0 || dropped != 0 || !r.svc.Tracker.IsReady() {
		t.Fatalf("replay = %d %d %v", read, dropped, err)
	}
	out, err := r.svc.Activity(context.Background(), r.now.Add(-time.Hour), r.now.Add(time.Minute), time.Minute, 20)
	if err != nil || out.Reasons[0].Count != 1 || len(out.TopCards) != 1 || out.TopCards[0].GPUIndex != 2 || out.TopCards[0].Node != "rn" {
		t.Fatalf("after the replay = %+v %v", out, err)
	}
	if d := r.svc.Deployments(r.now.Add(-time.Hour), r.now.Add(time.Minute), nil); d[dep.ID].Yielded != 1 || d[42].YieldedTo != 1 || d[dep.ID].HeldFrom.IsZero() {
		t.Fatalf("deployments = %+v", d)
	}
}

func yieldDeployment(name, policy string) *store.Deployment {
	dep := &store.Deployment{ModelID: 1, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive}
	dep.YieldPolicy = policy
	return dep
}
