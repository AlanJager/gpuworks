package app

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/controller"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The scheduling activity's reasons (store.SchedulingActivityReason) read
// the triggers and preemption reasons the automation spells itself; this
// pins them to its constants and yieldpolicy's.
func TestActivityReasonsReadTheAutomationEvents(t *testing.T) {
	for _, c := range []struct {
		code   string
		params map[string]any
		want   store.ActivityReason
	}{
		{automation.EventAutoSleep, map[string]any{"trigger": automation.TriggerPreempt, "reason": string(yieldpolicy.ReasonIdle)}, store.ActivityIdleYield},
		{automation.EventAutoSleep, map[string]any{"trigger": automation.TriggerPreempt, "reason": string(yieldpolicy.ReasonRank)}, store.ActivityRankPreemption},
		{automation.EventAutoSleep, map[string]any{"trigger": automation.TriggerPreempt, "reason": string(yieldpolicy.ReasonTurn)}, store.ActivityTimesliceTurn},
		{automation.EventAutoSleep, map[string]any{"trigger": automation.TriggerSchedule}, store.ActivityScheduled},
		{automation.EventAutoWake, map[string]any{"trigger": automation.TriggerSchedule}, store.ActivityScheduled},
		{automation.EventAutoScale, map[string]any{"trigger": automation.TriggerSchedule}, store.ActivityScheduled},
		{automation.EventYieldedForWake, map[string]any{"for_deployment": 3}, store.ActivityOperatorYield},
		{automation.EventWakeWaiting, map[string]any{"trigger": automation.TriggerWakeWaiting}, store.ActivityQueued},
		{automation.EventWakeWaitTimeout, map[string]any{}, store.ActivityFailure},
		{automation.EventScheduleActionFailed, map[string]any{}, store.ActivityFailure},
		{controller.EventNodeReplaced, map[string]any{}, store.ActivityFailure},
	} {
		data, err := json.Marshal(c.params)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := store.SchedulingActivityReason(c.code, string(data)); !ok || got != c.want {
			t.Errorf("%s %s = %q %v, want %q", c.code, data, got, ok, c.want)
		}
	}
	for _, trigger := range []string{automation.TriggerRequest, automation.TriggerResume, automation.TriggerWakeWaiting, automation.TriggerOperator} {
		data, _ := json.Marshal(map[string]any{"trigger": trigger})
		if got, ok := store.SchedulingActivityReason(automation.EventAutoWake, string(data)); ok {
			t.Errorf("a wake on %s counts as %q", trigger, got)
		}
	}
}
