package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// startDeployment brings a stopped deployment back: the controller schedules
// its replicas again from the configuration it kept. Stopping removed every
// instance, failed attempts included, so each replica starts with its whole
// retry budget. Starting an active deployment changes nothing.
func (a *apiHandlers) startDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	state, err := a.d.Store.StartDeployment(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if state == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	updated, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}

// restartRequest is POST /deployments/{id}/restart's optional body. The
// default strategy is rolling: replicas are replaced one at a time, each
// draining while its replacement starts. "all" stops every instance at once.
type restartRequest struct {
	Strategy string `json:"strategy,omitempty"`
}

const (
	restartRolling = "rolling"
	restartAll     = "all"
)

// restartDeployment restarts every replica of a deployment, whatever its
// shape (single-node replicas, multi-node replicas, PD roles), from the
// current network and image resolution; every replica gets a fresh retry
// budget, so failed attempts are retired either way.
func (a *apiHandlers) restartDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req restartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = restartRolling
	}
	if strategy != restartRolling && strategy != restartAll {
		writeProductError(w, http.StatusBadRequest, ErrorRestartStrategyInvalid, "strategy must be rolling or all", map[string]any{"strategy": req.Strategy})
		return
	}
	if dep.State != store.DeployActive {
		writeProductError(w, http.StatusConflict, ErrorDeploymentNotActive, "only an active deployment can be restarted", map[string]any{"state": string(dep.State)})
		return
	}
	if err := a.checkRestartStorage(dep); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if strategy == restartRolling {
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
		err = a.d.Store.RequestRollingRestart(dep.ID, time.Now().UTC())
		if a.restartRefused(w, dep, err) {
			return
		}
		a.operatorEvent(r, dep.ID, store.EventDeploymentRollingRestarted, map[string]any{"replicas": len(match.Slots)})
	} else {
		restarted, err := a.d.Store.RestartDeploymentNow(dep.ID)
		if a.restartRefused(w, dep, err) {
			return
		}
		a.operatorEvent(r, dep.ID, store.EventDeploymentRestarted, map[string]any{"instances": restarted})
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, a.deploymentDTO(updated, true))
}

// restartRefused writes the response for a restart the store refused and
// reports whether it did: the deployment stopped being active since it was
// read, or the write failed.
func (a *apiHandlers) restartRefused(w http.ResponseWriter, dep *store.Deployment, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrStateChanged):
		current := dep.State
		if fresh, getErr := a.d.Store.GetDeployment(dep.ID); getErr == nil {
			current = fresh.State
		}
		writeProductError(w, http.StatusConflict, ErrorDeploymentNotActive, "only an active deployment can be restarted", map[string]any{"state": string(current)})
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
	return true
}

// checkRestartStorage refuses a restart that would take a serving deployment
// down for good: an instance keeps serving while the storage it read from is
// unreachable (its weights are in GPU memory), but a replacement on that node
// could not load them, and a single-GPU restart removes the old instance
// before the new one starts. Whether a replacement could is the rule its
// launch follows (store.ModelSourceOn): refused there means refused here.
func (a *apiHandlers) checkRestartStorage(dep *store.Deployment) error {
	if dep.ModelID == 0 {
		return nil
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return err
	}
	rules, err := a.d.Store.SourceRulesFor(dep)
	if err != nil {
		return err
	}
	checked := map[uint]bool{}
	for _, inst := range insts {
		if checked[inst.NodeID] {
			continue
		}
		checked[inst.NodeID] = true
		node, err := a.d.Store.NodeByID(inst.NodeID)
		if err != nil {
			continue
		}
		src, err := a.d.Store.ModelSourceOn(dep.ModelID, node, rules)
		if err != nil {
			return err
		}
		if src.Action != store.SourceRefused {
			continue
		}
		storage, why := src.Location.Storage.Name, src.Reason
		if state := src.Location.NodeState; state != nil {
			why = state.State
		}
		name := nodeDisplayName(node)
		return productError(ErrorStorageUnavailableOnNode,
			"the storage "+storage+" is not ready on "+name+" ("+why+"); a restarted instance could not load the model",
			map[string]any{"storage": storage, "node": name, "state": why})
	}
	return nil
}
