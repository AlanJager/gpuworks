package store

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Each scheduling event counts under one reason, or none: the preemptions by
// their reason, the schedules' orders, the operator's yielding wake, the
// failures and the queued; a wake on demand, a placement and an operator's
// order count under none.
func TestSchedulingActivityReasons(t *testing.T) {
	for _, c := range []struct {
		code, data string
		want       ActivityReason
	}{
		{codeAutoSleep, `{"trigger":"preempt","reason":"idle"}`, ActivityIdleYield},
		{codeAutoSleep, `{"trigger":"preempt","reason":"rank"}`, ActivityRankPreemption},
		{codeAutoSleep, `{"trigger":"preempt"}`, ActivityRankPreemption},
		{codeAutoSleep, `{"trigger":"preempt","reason":"turn"}`, ActivityTimesliceTurn},
		{codeAutoSleep, `{"trigger":"schedule","schedule_id":3}`, ActivityScheduled},
		{codeAutoSleep, `not json`, ""},
		{codeAutoWake, `{"trigger":"schedule"}`, ActivityScheduled},
		{codeAutoWake, `{"trigger":"request"}`, ""},
		{codeAutoWake, `{"trigger":"wake_waiting"}`, ""},
		{"AUTO_SCALE", `{}`, ActivityScheduled},
		{codeYieldedForWake, `{"deployment":"a"}`, ActivityOperatorYield},
		{"WAKE_WAITING", `{}`, ActivityQueued},
		{EventDeploymentUnschedulable, `{}`, ActivityQueued},
		{"WAKE_WAIT_TIMEOUT", `{}`, ActivityFailure},
		{"SCHEDULE_ACTION_FAILED", `{}`, ActivityFailure},
		{EventReplicaRebuilt, `{}`, ActivityFailure},
		{"NODE_REPLACED", `{}`, ActivityFailure},
		{"NODE_REPLACE_NO_CANDIDATE", `{}`, ActivityFailure},
		{EventUnitRetryExhausted, `{}`, ActivityFailure},
		{EventDeploymentRollingStalled, `{}`, ActivityFailure},
		{"UNIT_PLACED", `{}`, ""},
		{"NODE_DRAIN_MOVED", `{}`, ""},
		{EventSleepOrdered, `{}`, ""},
		{"INSTANCE_STATUS_UPDATED", `{}`, ""},
	} {
		got, ok := SchedulingActivityReason(c.code, c.data)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s %s = %q %v, want %q", c.code, c.data, got, ok, c.want)
		}
	}
	if len(ActivityReasons()) != 7 || ActivityReasonIndex(ActivityQueued) != 6 || ActivityReasonIndex("nope") != -1 {
		t.Fatalf("reasons = %v", ActivityReasons())
	}
}

// The observer is handed each event the activity counts once it is
// committed: a yield with the cards of the instances it names and the
// deployment it was for, the state events of an instance's life from its
// creation to its removal; an event it has no use for is not handed over.
func TestEventObserverSeesCommittedEvents(t *testing.T) {
	s, node, ids := seedResidency(t)
	var seen []ObservedEvent
	s.SetEventObserver(func(ev ObservedEvent) { seen = append(seen, ev) })
	inst, err := s.CreateInstance(&Instance{UUID: "obs-1", DeploymentID: ids["alpha"], NodeID: node.ID, GPUIndexes: "[1,0]"})
	if err != nil {
		t.Fatal(err)
	}
	s.AddDeploymentEventCode(ids["alpha"], EventKindAutomation, codeAutoSleep, map[string]any{"trigger": "preempt", "reason": "turn", "for_deployment": ids["beta"], "instances": []string{"obs-1"}})
	s.AddDeploymentEventCode(ids["alpha"], EventKindLifecycle, EventSleepOrdered, map[string]any{"instances": []string{"obs-1"}})
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "obs-1", State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteInstance(inst.UUID); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, ev := range seen {
		if ev.ID == 0 || ev.At.IsZero() {
			t.Fatalf("handed over before it was written: %+v", ev)
		}
		if ev.Residency != nil {
			codes = append(codes, ev.Residency.Code)
			continue
		}
		codes = append(codes, "yield")
		if !ev.Yield || !ev.Counted || ev.Reason != ActivityTimesliceTurn || ev.ForDeployment != ids["beta"] || ev.DeploymentID != ids["alpha"] {
			t.Fatalf("yield = %+v", ev)
		}
		if fmt.Sprint(ev.Cards) != fmt.Sprintf("[{%s 0} {%s 1}]", node.UUID, node.UUID) {
			t.Fatalf("cards = %+v, want the instance's two, sorted", ev.Cards)
		}
	}
	if fmt.Sprint(codes) != "[INSTANCE_CREATED yield INSTANCE_STATUS_UPDATED INSTANCE_REMOVED]" {
		t.Fatalf("handed over %v", codes)
	}
	s.SetEventObserver(nil)
	s.AddDeploymentEventCode(ids["alpha"], EventKindAutomation, codeAutoWake, map[string]any{"trigger": "request"})
	if len(seen) != 4 {
		t.Fatal("a removed observer was still called")
	}
}

// The replay hands over the residency of the deployments that share cards,
// each group in the order it happened, the state before the window dated at
// its start, and every other instance's row dated now.
func TestReplayHandsOverResidencyInOrder(t *testing.T) {
	s, node, ids := seedResidency(t)
	never, err := s.CreateDeployment(withPolicy(&Deployment{ModelID: 1, NodeID: node.ID, Name: "resident", ServedName: "resident", Engine: "vllm", State: DeployActive}, yieldpolicy.Never))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, in := range []Instance{
		{UUID: "a-1", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: now.Add(-30 * time.Hour)},
		{UUID: "r-1", DeploymentID: never.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: 1, GPUIndexes: "[1]", CreatedAt: now.Add(-30 * time.Hour)},
	} {
		in := in
		if err := s.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	putEvent(t, s, "a-1", ids["alpha"], "state", instanceEventStatusUpdated, now.Add(-29*time.Hour), `{"state":"running"}`)
	putEvent(t, s, "a-1", ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-2*time.Hour), `{"result":"applied","level":1}`)
	since := now.Add(-6 * time.Hour)
	through, _ := s.LastEventID()
	var groups [][]ObservedEvent
	if err := s.ReplaySchedulingActivity(ActivityReplay{Since: since, ThroughID: through, Residency: func(evs []ObservedEvent) { groups = append(groups, evs) }}, now); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want the shared deployments' and the other rows'", len(groups))
	}
	shared := groups[0]
	if len(shared) != 2 || shared[0].InstanceUUID != "a-1" || !shared[0].At.Equal(since) || !shared[1].At.Equal(now.Add(-2*time.Hour)) || shared[1].Residency.Level != 1 {
		t.Fatalf("shared = %+v", shared)
	}
	rows := groups[1]
	if len(rows) != 2 || rows[0].InstanceUUID != "r-1" || !rows[0].At.Equal(now) || rows[1].Residency.Level != 1 {
		t.Fatalf("other rows = %+v", rows)
	}
	var m ResidencyMachine
	for _, ev := range rows {
		m.Apply(*ev.Residency)
	}
	if state, ok := m.Label(); !ok || state != ResidencyAsleep {
		t.Fatalf("seeded machine = %v %v", state, ok)
	}
}

// Presence runs from creation (or the first card held, if earlier) to now,
// or to the deletion; a deleted deployment's runs from its first card held.
func TestPresence(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	from := now.Add(-6 * time.Hour)
	at := func(h int) time.Time { return now.Add(time.Duration(h) * time.Hour) }
	for _, c := range []struct {
		info               ResidencyDeployment
		seenFrom, seenTo   time.Time
		wantFrom, wantTill time.Time
	}{
		{ResidencyDeployment{Since: at(-24)}, at(-5), at(0), from, now},
		{ResidencyDeployment{Since: at(-3)}, at(-2), at(0), at(-3), now},
		{ResidencyDeployment{Deleted: true, Until: at(-1)}, at(-4), at(-2), at(-4), at(-1)},
		{ResidencyDeployment{Deleted: true}, at(-4), at(-2), at(-4), at(-2)},
	} {
		start, end := Presence(c.info, c.seenFrom, c.seenTo, from, now)
		if !start.Equal(c.wantFrom) || !end.Equal(c.wantTill) {
			t.Errorf("%+v: %v..%v, want %v..%v", c.info, start, end, c.wantFrom, c.wantTill)
		}
	}
	if !ListedInSharing(ResidencyDeployment{Yield: yieldpolicy.Idle}, ResidencySwitches{}) || ListedInSharing(ResidencyDeployment{Yield: yieldpolicy.Never}, ResidencySwitches{Yielded: 1}) ||
		ListedInSharing(ResidencyDeployment{Deleted: true}, ResidencySwitches{Wakes: 2}) || !ListedInSharing(ResidencyDeployment{Deleted: true}, ResidencySwitches{YieldedTo: 1}) {
		t.Fatal("listed in sharing")
	}
}
