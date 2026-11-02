package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The statistics hook the scheduling activity to the store before anything
// is written: an event written while the replay has not run is counted once
// it has, and the last flush writes the hour in progress to stats.db.
func TestOpsStatsCountsTheSchedulingActivity(t *testing.T) {
	now := time.Now().UTC()
	ops, st, _ := newTestOpsStats(t, &now)
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: 1, Name: "d", ServedName: "d", Engine: "vllm", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{})
	ops.replayScheduling(context.Background())
	st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{})
	out, err := ops.sched.Activity(context.Background(), now.Add(-time.Hour), now.Add(time.Minute), time.Minute, 20)
	if err != nil {
		t.Fatal(err)
	}
	if queued := out.Reasons[store.ActivityReasonIndex(store.ActivityQueued)].Count; queued != 2 {
		t.Fatalf("queued = %d, want the event replayed and the one observed after", queued)
	}
	ops.finalFlush()
	rows, err := ops.stats.SchedulingReasons(context.Background(), statsdb.Range{From: now.Truncate(time.Hour).UnixMilli(), To: now.Add(time.Hour).UnixMilli()})
	if err != nil || len(rows) != 1 || rows[0].Events != 2 || rows[0].Reason != string(store.ActivityQueued) {
		t.Fatalf("written = %+v %v", rows, err)
	}
}
