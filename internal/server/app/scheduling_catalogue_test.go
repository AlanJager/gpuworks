package app

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/controller"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The scheduling history's catalogue (store.SchedulingEventCodes) lists codes
// that the controller, the automation and the API record; the store cannot
// import the first two, so this pins every code they define to the catalogue
// with the kind they write it with. A new scheduling event that is not added
// there would silently be missing from the scheduling center.
func TestSchedulingCatalogueCoversTheRecordedCodes(t *testing.T) {
	have := map[string]store.SchedulingEventCode{}
	for _, entry := range store.SchedulingEventCodes() {
		have[entry.Code] = entry
	}
	for _, want := range []struct {
		code, kind string
		category   store.SchedulingCategory
	}{
		{automation.EventAutoSleep, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventAutoWake, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventAutoScale, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventWakeWaiting, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventWakeWaitTimeout, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventScheduleActionFailed, store.EventKindAutomation, store.SchedulingAuto},
		{automation.EventYieldedForWake, store.EventKindAutomation, store.SchedulingAuto},
		{controller.EventUnitPlaced, store.EventKindAutomation, store.SchedulingAuto},
		{controller.EventNodeDrainMoved, store.EventKindAutomation, store.SchedulingAuto},
		{controller.EventNodeReplaced, store.EventKindAutomation, store.SchedulingFailure},
		{controller.EventNodeReplaceReverted, store.EventKindAutomation, store.SchedulingFailure},
		{controller.EventNodeReplaceNoCandidate, store.EventKindAutomation, store.SchedulingFailure},
		{store.EventReplicaRebuilt, store.EventKindLifecycle, store.SchedulingFailure},
		{store.EventUnitRetryExhausted, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventDeploymentUnschedulable, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventDeploymentRollingStalled, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventReplicasChanged, store.EventKindLifecycle, store.SchedulingManual},
		{store.EventReplicaRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventUnitRetried, store.EventKindSchedule, store.SchedulingManual},
		{store.EventDeploymentRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventDeploymentRollingRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventSleepOrdered, store.EventKindLifecycle, store.SchedulingManual},
		{store.EventWakeOrdered, store.EventKindLifecycle, store.SchedulingManual},
	} {
		entry, ok := have[want.code]
		if !ok || entry.Kind != want.kind || entry.Category != want.category {
			t.Errorf("%s: catalogue has %+v (listed %v), want kind %s category %s", want.code, entry, ok, want.kind, want.category)
		}
	}
}

// The store counts yields and wakes (store.IsYield, store.IsAutoWake,
// store.EventForDeployment) from the automation's events by the codes and
// trigger it spells itself; this pins those spellings to the automation's
// constants by feeding the store events written as the automation writes
// them (residency.go's preemption, wake_wait.go's yielding wake).
func TestStoreYieldClassifierReadsTheAutomationEvents(t *testing.T) {
	preempt, err := json.Marshal(map[string]any{"trigger": automation.TriggerPreempt, "reason": "rank", "for_deployment": uint(7), "instances": []string{"i-1"}})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _ := json.Marshal(map[string]any{"trigger": automation.TriggerSchedule, "schedule_id": 3})
	for _, c := range []struct {
		ev          store.InstanceEvent
		yield, wake bool
		forDep      uint
	}{
		{store.InstanceEvent{Code: automation.EventAutoSleep, Data: string(preempt)}, true, false, 7},
		{store.InstanceEvent{Code: automation.EventYieldedForWake, Data: `{"deployment":"alpha","instances":["i-2"],"level":1}`}, true, false, 0},
		{store.InstanceEvent{Code: automation.EventAutoSleep, Data: string(scheduled)}, false, false, 0},
		{store.InstanceEvent{Code: automation.EventAutoWake, Data: `{"trigger":"request"}`}, false, true, 0},
		{store.InstanceEvent{Code: automation.EventAutoScale, Data: `{}`}, false, false, 0},
	} {
		if got := store.IsYield(c.ev); got != c.yield {
			t.Errorf("IsYield(%s %s) = %v, want %v", c.ev.Code, c.ev.Data, got, c.yield)
		}
		if got := store.IsAutoWake(c.ev); got != c.wake {
			t.Errorf("IsAutoWake(%s) = %v, want %v", c.ev.Code, got, c.wake)
		}
		if got := store.EventForDeployment(c.ev.Data); got != c.forDep {
			t.Errorf("EventForDeployment(%s) = %d, want %d", c.ev.Data, got, c.forDep)
		}
	}
}
