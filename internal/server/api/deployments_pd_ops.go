package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pdUpdateRequest is PUT /deployments/{id}/pd: each role's units and the
// gateway fallback may change while the deployment runs. What an instance
// was launched with (mode, a role's TP and PP, the KV load failure policy) may
// not:
// changing it means new instances, which is a clone.
type pdUpdateRequest struct {
	Mode          string                     `json:"mode,omitempty"`
	Roles         map[string]planning.PDRole `json:"roles,omitempty"`
	KVLoadFailure string                     `json:"kv_load_failure,omitempty"`
	Fallback      *string                    `json:"fallback,omitempty"`
}

// pdDeployment loads a deployment and its PD placement.
func (a *apiHandlers) pdDeployment(w http.ResponseWriter, r *http.Request) (*store.Deployment, planning.Placement, bool) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, planning.Placement{}, false
	}
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, planning.Placement{}, false
	}
	if placement.PD == nil {
		writeProductError(w, http.StatusBadRequest, messages.ErrorPDNotPD, "deployment is not PD disaggregated", nil)
		return nil, planning.Placement{}, false
	}
	return dep, placement, true
}

// applyPDUpdate applies a PD update to a running deployment and stores the
// placement it moves to: the nodes of new units are filled in for a
// deployment the platform places, the result is checked against the mode and
// the nodes, and the placement is written. It is the one path PUT
// /deployments/{id}/pd and a scheduled role count take. The status is the
// HTTP status of a refusal.
func (a *apiHandlers) applyPDUpdate(dep *store.Deployment, current *planning.PDPlacement, req *pdUpdateRequest) (*planning.PDPlacement, int, error) {
	if dep.UnitsAuto && len(req.Roles) > 0 {
		if status, err := a.autoPDRoles(dep, current, req.Roles); err != nil {
			return nil, status, err
		}
	}
	applied, err := planning.ApplyPDUpdate(current, dep.Engine, planning.PDUpdate{Mode: req.Mode, Roles: req.Roles, KVLoadFailure: req.KVLoadFailure, Fallback: req.Fallback})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	next := applied.Placement
	nodes := map[string]*store.Node{}
	var nodeIDs []uint
	for _, uuid := range next.NodeUUIDs(applied.Mode) {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // ValidatePDNodes names the missing node
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		nodes[uuid] = node
		nodeIDs = append(nodeIDs, node.ID)
	}
	if _, err := planning.ValidatePDNodes(next, applied.Mode, nodes, false); err != nil {
		return nil, http.StatusBadRequest, err
	}
	encoded, err := planning.WithPD(dep.Placement, next)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	primary := dep.NodeID
	if len(nodeIDs) > 0 {
		primary = nodeIDs[0]
	}
	if err := a.d.Store.SetDeploymentPlacement(dep.ID, encoded, jsonStr(nodeIDs), primary); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return next, 0, nil
}

// updatePD scales a PD deployment's roles and sets its fallback (plan phase
// 3). Removed units drain before their instances stop; a role the mode may
// empty (prefill) may go to zero and come back. Scaling up waits while a
// group restart is required.
func (a *apiHandlers) updatePD(w http.ResponseWriter, r *http.Request) {
	dep, placement, ok := a.pdDeployment(w, r)
	if !ok {
		return
	}
	var req pdUpdateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	next, status, err := a.applyPDUpdate(dep, placement.PD, &req)
	if err != nil {
		writeErr(w, status, err)
		return
	}
	roles := make([]string, 0, len(req.Roles))
	for role := range req.Roles {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if from, to := len(placement.PD.Units(role)), len(next.Units(role)); from != to {
			a.operatorEvent(r, dep.ID, store.EventReplicasChanged, map[string]any{"role": role, "from": from, "to": to})
		}
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}
