package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func unschedulableFixture(t *testing.T) (*Controller, *store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dep, err := createTestDeployment(t, st, &store.Deployment{Name: "blocked", NodeID: 1, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	// The hub is not touched on this path: recording a placement failure is a
	// store + queue operation.
	return New(st, nil, nil), st, dep
}

// The scheduler's reason is already the structured, locale-neutral unit
// (scheduler.Reason documents itself as feeding instance_events). It used to be
// written to one log line and dropped, which is what left a deployment that
// could never be placed permanently invisible.
func TestRecordUnschedulablePublishesTheSchedulerReason(t *testing.T) {
	c, st, dep := unschedulableFixture(t)

	reason := scheduler.Reason{
		Code:    scheduler.ReasonNoFreeGPU,
		Params:  map[string]any{"tp": 2, "free": 0},
		Message: "no free gpu for tp=2",
	}
	c.recordUnschedulable(dep, reason)

	ev, found, err := st.FirstScheduleEvent(dep.ID)
	if err != nil || !found {
		t.Fatalf("FirstScheduleEvent found=%v err=%v", found, err)
	}
	if ev.Kind != store.EventKindSchedule || ev.Code != store.EventDeploymentUnschedulable {
		t.Fatalf("event not classified as a scheduling outcome: %+v", ev)
	}
	if ev.InstanceID != 0 || ev.InstanceUUID != "" {
		t.Fatalf("a scheduling outcome has no instance to hang off: %+v", ev)
	}
	var params struct {
		Scheduler scheduler.Reason `json:"scheduler"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &params); err != nil {
		t.Fatalf("event data is not the scheduler reason: %q (%v)", ev.Data, err)
	}
	if params.Scheduler.Code != scheduler.ReasonNoFreeGPU {
		t.Fatalf("reason code lost: %+v", params.Scheduler)
	}
	if params.Scheduler.Params["tp"] != float64(2) {
		t.Fatalf("reason params lost: %+v", params.Scheduler.Params)
	}

	// Re-checking is the other half: the work queue is purely event-driven, so
	// without a self-scheduled retry nothing would ever reconcile this
	// deployment again.
	c.q.mu.Lock()
	_, queued := c.q.pending[depKey(dep.ID)]
	c.q.mu.Unlock()
	if !queued {
		t.Fatal("no retry was scheduled; the deployment would never be re-checked")
	}
}

// Reconciling is cheap and runs on every node/model/deployment change. Writing
// an event per attempt would bury the timeline in duplicates and turn "still
// unschedulable" into noise.
func TestRecordUnschedulableThrottlesWithinTheWindow(t *testing.T) {
	c, st, dep := unschedulableFixture(t)
	reason := scheduler.Reason{Code: scheduler.ReasonNoFit, Message: "no fit"}

	c.recordUnschedulable(dep, reason)
	c.recordUnschedulable(dep, reason)
	c.recordUnschedulable(dep, reason)

	events, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1: repeated refusals inside one window are the same fact", len(events))
	}

	// A refusal older than the window means the situation has persisted and is
	// worth recording again - this is the periodic re-announcement.
	if err := st.DB().Model(&store.InstanceEvent{}).
		Where("deployment_id = ?", dep.ID).
		Update("created_at", time.Now().Add(-2*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	c.recordUnschedulable(dep, reason)

	events, err = st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: a persisting refusal must be re-announced", len(events))
	}
}

// A different refusal is written at once rather than after the window: the
// latest event is what the deployment's status shows, so one recorded while
// the agents were reconnecting ("every node offline") must give way to the
// real reason as soon as there is one. Two refusals still sit at least
// refusalChangeFloor apart, so a flapping deployment cannot write a row per
// pass; and the same code with other counts is the same refusal.
func TestRecordUnschedulableWritesAChangedReasonAtOnce(t *testing.T) {
	c, st, dep := unschedulableFixture(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	offline := scheduler.Reason{Code: scheduler.ReasonPoolEmpty, Params: map[string]any{"offline": 3}}
	c.recordUnschedulable(dep, offline)

	now = now.Add(refusalChangeFloor / 2)
	noFit := scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: map[string]any{"tried": 3}}
	c.recordUnschedulable(dep, noFit)
	if events, _ := st.DeploymentEvents(dep.ID, 0); len(events) != 1 {
		t.Fatalf("events = %d, want 1: a change inside the floor waits", len(events))
	}
	c.q.mu.Lock()
	due, queued := c.q.pending[depKey(dep.ID)]
	c.q.mu.Unlock()
	if !queued || due.After(time.Now().Add(refusalChangeFloor)) {
		t.Fatalf("re-check due %v queued=%v, want within the floor", due, queued)
	}

	now = now.Add(refusalChangeFloor)
	c.recordUnschedulable(dep, noFit)
	latest, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(latest.Data, `"code":"pool_no_fit"`) {
		t.Fatalf("latest = %+v found=%v err=%v, want the changed reason", latest, found, err)
	}

	now = now.Add(refusalChangeFloor)
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: map[string]any{"tried": 8, "busy": 5}})
	if events, _ := st.DeploymentEvents(dep.ID, 0); len(events) != 2 {
		t.Fatalf("events = %d, want 2: how busy the candidates are does not make another refusal", len(events))
	}

	// The pool's best candidate refusing for another reason is a change: a
	// replica held back by a stuck model, then by full cards once the model
	// arrived, must not keep blaming the model.
	now = now.Add(refusalChangeFloor)
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: map[string]any{"tried": 3, "first": scheduler.ReasonModelUnavailable}})
	now = now.Add(refusalChangeFloor)
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: map[string]any{"tried": 3, "first": scheduler.ReasonNoFreeGPU}})
	latest, _, _ = st.LatestScheduleEvent(dep.ID)
	if events, _ := st.DeploymentEvents(dep.ID, 0); len(events) != 4 || !strings.Contains(latest.Data, `"first":"no_free_gpu"`) {
		t.Fatalf("events = %d, latest %s: want the best candidate's new refusal written", len(events), latest.Data)
	}

	// Where the deployment may run changing is news too: requirements edited
	// to exclude a node, or a node cordoned or gone, shows at once.
	now = now.Add(refusalChangeFloor)
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPoolNoFit, Params: map[string]any{"tried": 2, "first": scheduler.ReasonNoFreeGPU, "unmet": 1}})
	latest, _, _ = st.LatestScheduleEvent(dep.ID)
	if events, _ := st.DeploymentEvents(dep.ID, 0); len(events) != 5 || !strings.Contains(latest.Data, `"unmet":1`) {
		t.Fatalf("events = %d, latest %s: want the narrowed pool written", len(events), latest.Data)
	}
}

// Just after the server starts, nodes read offline until their agents
// reconnect. A refusal that names offline nodes in that window is not
// recorded (the deployment is looked at again soon); after it, or with no
// offline node named, it is.
func TestARefusalForOfflineNodesWhileAgentsReconnectIsNotRecorded(t *testing.T) {
	c, st, dep := unschedulableFixture(t)
	start := time.Date(2026, 10, 29, 14, 16, 52, 0, time.UTC)
	clock := start.Add(time.Second)
	c.now = func() time.Time { return clock }
	c.watchingSince = start
	offline := scheduler.Reason{Code: scheduler.ReasonPoolEmpty, Params: map[string]any{"nodes": 1, "offline": 1}}
	// A node that has not reported since the server started: its agent is
	// reconnecting.
	node, err := st.RegisterNode("reconnecting", store.HashToken("t"), "reconnecting")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("last_seen_at", start.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	c.recordUnschedulable(dep, offline)
	if _, found, err := st.FirstScheduleEvent(dep.ID); err != nil || found {
		t.Fatalf("a refusal while the agents reconnect was recorded (found=%v err=%v)", found, err)
	}
	c.q.mu.Lock()
	_, queued := c.q.pending[depKey(dep.ID)]
	c.q.mu.Unlock()
	if !queued {
		t.Fatal("the deployment is not looked at again")
	}

	// Once every node has reported since the start, an offline node went
	// offline for real and the refusal stands even within the grace.
	seen := start.Add(time.Second)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("last_seen_at", seen).Error; err != nil {
		t.Fatal(err)
	}
	c.recordUnschedulable(dep, offline)
	if _, found, _ := st.FirstScheduleEvent(dep.ID); !found {
		t.Fatal("a refusal for a node that went offline after reconnecting was held back")
	}

	// A refusal that names no offline node is recorded at once.
	c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonNoFreeGPU, Params: map[string]any{"free": 0}})
	if _, found, _ := st.FirstScheduleEvent(dep.ID); !found {
		t.Fatal("a refusal unrelated to reconnecting agents was held back")
	}

	// Past the grace the offline refusal stands.
	c2, st2, dep2 := unschedulableFixture(t)
	c2.now = func() time.Time { return start.Add(c2.offlineGrace) }
	c2.watchingSince = start
	c2.recordUnschedulable(dep2, offline)
	if _, found, _ := st2.FirstScheduleEvent(dep2.ID); !found {
		t.Fatal("an offline refusal after the grace was not recorded")
	}
}
