package app

import (
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
		{store.EventReplicaRebuilt, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventUnitRetryExhausted, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventDeploymentUnschedulable, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventDeploymentRollingStalled, store.EventKindSchedule, store.SchedulingFailure},
		{store.EventReplicasChanged, store.EventKindSchedule, store.SchedulingManual},
		{store.EventReplicaRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventUnitRetried, store.EventKindSchedule, store.SchedulingManual},
		{store.EventDeploymentRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventDeploymentRollingRestarted, store.EventKindSchedule, store.SchedulingManual},
		{store.EventSleepOrdered, store.EventKindSchedule, store.SchedulingManual},
		{store.EventWakeOrdered, store.EventKindSchedule, store.SchedulingManual},
	} {
		entry, ok := have[want.code]
		if !ok || entry.Kind != want.kind || entry.Category != want.category {
			t.Errorf("%s: catalogue has %+v (listed %v), want kind %s category %s", want.code, entry, ok, want.kind, want.category)
		}
	}
}
