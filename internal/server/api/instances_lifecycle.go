package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// restartInstance replaces the generation of the replica an instance fills.
// A follower names its whole generation: a collective cannot replace one
// process. By default the leader drains (the gateway sends it nothing new and
// it is retired once idle) while the controller starts the replacement;
// ?drain=false removes it at once. Either way it costs the replica no retry.
func (a *apiHandlers) restartInstance(w http.ResponseWriter, r *http.Request) {
	uuid := chi.URLParam(r, "uuid")
	drain := true
	if raw := r.URL.Query().Get("drain"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		drain = parsed
	}
	inst, err := a.d.Store.InstanceByUUID(uuid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	dep, err := a.d.Store.GetDeployment(inst.DeploymentID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if dep.State != store.DeployActive {
		writeProductError(w, http.StatusConflict, ErrorDeploymentNotActive, "only an active deployment's instances can be restarted", map[string]any{"state": string(dep.State)})
		return
	}
	if err := a.checkRestartStorage(dep); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	match, err := a.deploymentUnitMatch(dep, insts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	slot := match.SlotOf(uuid)
	if slot == nil {
		writeProductError(w, http.StatusConflict, ErrorInstanceNotServing, "the instance fills no replica: it failed, is draining or is being removed", map[string]any{"state": inst.State})
		return
	}
	leader := slot.Inst
	// Without draining the whole generation goes now; the followers would be
	// removed as orphans once the leader is gone anyway.
	to, uuids := proto.InstDraining, []string{leader.UUID}
	if !drain {
		to = proto.InstRemoving
		for _, follower := range slot.Followers {
			if follower != nil {
				uuids = append(uuids, follower.UUID)
			}
		}
	}
	err = a.d.Store.TransitionInstances(dep.ID, uuids, store.ReplicaFillingStates(), to, "restart requested")
	if errors.Is(err, store.ErrStateChanged) {
		// The state that refused the write, not the one read before it.
		state := inst.State
		if current, readErr := a.d.Store.InstanceByUUID(inst.UUID); readErr == nil {
			state = current.State
		}
		writeProductError(w, http.StatusConflict, ErrorInstanceNotServing, "the instance fills no replica: it failed, is draining or is being removed", map[string]any{"state": state})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.operatorEvent(r, dep.ID, store.EventReplicaRestarted, map[string]any{"instance": leader.UUID, "drain": drain})
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, a.deploymentDTO(updated, true))
}
