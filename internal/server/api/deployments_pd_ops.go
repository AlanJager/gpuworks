package api

import (
	"encoding/json"
	"errors"
	"net/http"

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
	applied, err := planning.ApplyPDUpdate(placement.PD, dep.Engine, planning.PDUpdate{Mode: req.Mode, Roles: req.Roles, KVLoadFailure: req.KVLoadFailure, Fallback: req.Fallback})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
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
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		nodes[uuid] = node
		nodeIDs = append(nodeIDs, node.ID)
	}
	if _, err := planning.ValidatePDNodes(next, applied.Mode, nodes, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	encoded, err := planning.WithPD(dep.Placement, next)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	primary := dep.NodeID
	if len(nodeIDs) > 0 {
		primary = nodeIDs[0]
	}
	if err := a.d.Store.SetDeploymentPlacement(dep.ID, encoded, jsonStr(nodeIDs), primary); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}
