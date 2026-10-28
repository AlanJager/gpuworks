package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// automationEventKind is the timeline kind of the scheduling center's
// events, the automation's own and an operator's waiting or yielding wake.
const automationEventKind = store.EventKindAutomation

// wakeWaitHow is how a wake that does not fit proceeds: wait waits for room
// (the automation wakes the sleepers once their GPUs can take them back);
// yield first puts the awake occupants of those GPUs to sleep at yieldLevel,
// then waits.
type wakeWaitHow struct {
	wait, yield bool
	yieldLevel  int
}

// wakeWaitParams reads ?wait=, ?yield= and ?yield_level= (1 or 2, default 1),
// writing the response when they are invalid.
func wakeWaitParams(w http.ResponseWriter, r *http.Request) (wakeWaitHow, bool) {
	how := wakeWaitHow{yieldLevel: proto.SleepLevelL1}
	for name, into := range map[string]*bool{"wait": &how.wait, "yield": &how.yield} {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return how, false
		}
		*into = parsed
	}
	switch raw := r.URL.Query().Get("yield_level"); raw {
	case "", "1":
	case "2":
		how.yieldLevel = proto.SleepLevelL2
	default:
		writeProductError(w, http.StatusBadRequest, ErrorSleepInvalidLevel, "sleep level must be 1 or 2", map[string]any{"level": raw})
		return how, false
	}
	return how, true
}

// wakeAndWait wakes the named sleepers that fit now and leaves the rest
// waiting for room; with yield, it first puts the awake occupants of the GPUs
// they need to sleep. A yield whose occupants cannot all sleep changes
// nothing (409). The caller holds the lifecycle lock.
func (a *apiHandlers) wakeAndWait(w http.ResponseWriter, dep *store.Deployment, match planning.UnitMatch, named []store.Instance, whole bool, how wakeWaitHow) {
	woken, refused, err := planning.OrderWake(a.d.Store, dep, match, named, true, whole)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	result := dto.WakeWaitResult{Status: "waking", Woken: uuidsOf(woken), Waiting: []string{}, Yielded: []string{}}
	if len(refused) == 0 {
		writeJSON(w, http.StatusOK, result)
		return
	}
	if how.yield {
		targets, blockers, err := planning.YieldTargets(a.d.Store, refused)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if len(blockers) > 0 {
			names := make([]string, 0, len(blockers))
			for _, b := range blockers {
				name := b.Deployment
				if name == "" {
					name = "process outside the platform"
				}
				names = appendUnique(names, name)
			}
			writeProductError(w, http.StatusConflict, messages.ErrorWakeYieldBlocked,
				fmt.Sprintf("cannot make room: %s cannot be put to sleep", strings.Join(names, ", ")),
				map[string]any{"occupants": names, "woken": result.Woken})
			return
		}
		for _, target := range targets {
			level := how.yieldLevel
			members := []store.Instance{target.Instance}
			if level == proto.SleepLevelL1 && planning.L1HostMemoryRefusal(a.d.Store, target.Deployment, members, a.hostMemory) != nil {
				// The occupant's host cannot hold its weights: it gives way at
				// L2, which needs no host memory.
				level = proto.SleepLevelL2
			}
			targetInsts, err := a.d.Store.InstancesForDeployment(target.Deployment.ID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			targetMatch, err := planning.DeploymentMatch(a.d.Store, target.Deployment, targetInsts)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			replica := planning.ReplicaMembers(targetMatch.SlotOf(target.Instance.UUID))
			if err := a.d.Store.OrderInstancesSleep(target.Deployment.ID, uuidsOf(replica), level, proto.SleepModeWait); err != nil {
				writeReplicaOrderErr(w, err)
				return
			}
			a.d.Store.AddDeploymentEventCode(target.Deployment.ID, automationEventKind, automation.EventYieldedForWake, map[string]any{
				"deployment": dep.Name, "instances": uuidsOf(replica), "level": level,
			})
			result.Yielded = appendUnique(result.Yielded, target.Deployment.Name)
		}
	}
	for _, r := range refused {
		result.Waiting = append(result.Waiting, uuidsOf(r.Instances)...)
	}
	if err := a.d.Store.MarkWakeWaiting(dep.ID, result.Waiting, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.d.Store.AddDeploymentEventCode(dep.ID, automationEventKind, automation.EventWakeWaiting, map[string]any{"trigger": automation.TriggerOperator, "instances": result.Waiting, "yielded": result.Yielded})
	result.Status = "waiting"
	writeJSON(w, http.StatusAccepted, result)
}
