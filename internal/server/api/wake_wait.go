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
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// wakeWaitHow is how a wake that does not fit proceeds: wait waits for room
// (the automation wakes the sleepers once their GPUs can take them back);
// yield first puts the awake occupants of those GPUs to sleep at yieldPref,
// then waits. An operator's yield_level=1 is the platform's choice
// (servicelevel.LevelAuto): an occupant whose host cannot hold its weights
// gives way at L2, which needs no host memory, rather than refusing the wake.
type wakeWaitHow struct {
	wait, yield bool
	yieldPref   string
}

// wakeWaitParams reads ?wait=, ?yield= and ?yield_level= (1 or 2, default 1),
// writing the response when they are invalid.
func wakeWaitParams(w http.ResponseWriter, r *http.Request) (wakeWaitHow, bool) {
	how := wakeWaitHow{yieldPref: servicelevel.LevelAuto}
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
		how.yieldPref = servicelevel.LevelL2
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
func (a *apiHandlers) wakeAndWait(w http.ResponseWriter, r *http.Request, dep *store.Deployment, match planning.UnitMatch, named []store.Instance, whole bool, how wakeWaitHow) {
	woken, refused, err := planning.OrderWake(a.d.Store, dep, match, named, true, whole)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	result := dto.WakeWaitResult{Status: "waking", Woken: uuidsOf(woken), Waiting: []string{}, Yielded: []string{}}
	a.wakeOrdered(r, dep.ID, result.Woken)
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
			level, refusal, err := planning.OrderSleep(a.d.Store, target.Deployment, planning.SleepOrder{
				Pref: how.yieldPref, Mode: proto.SleepModeWait, Held: true, Instances: uuidsOf(target.Members),
			}, a.hostMemory)
			if err != nil {
				writeReplicaOrderErr(w, err)
				return
			}
			if refusal != nil {
				writeSleepRefusal(w, http.StatusConflict, refusal)
				return
			}
			a.d.Store.AddDeploymentEventCode(target.Deployment.ID, store.EventKindAutomation, automation.EventYieldedForWake, map[string]any{
				"deployment": dep.Name, "for_deployment": dep.ID, "instances": uuidsOf(target.Members), "level": level,
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
	a.d.Store.AddDeploymentEventCode(dep.ID, store.EventKindAutomation, automation.EventWakeWaiting, map[string]any{"trigger": automation.TriggerOperator, "instances": result.Waiting, "yielded": result.Yielded})
	result.Status = "waiting"
	writeJSON(w, http.StatusAccepted, result)
}
