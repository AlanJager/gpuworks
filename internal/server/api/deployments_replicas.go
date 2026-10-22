package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replicasRequest is PUT /deployments/{id}/replicas, the one scaling entry of
// an ordinary deployment. A single-node deployment sends a count and, when it
// shrinks, may name the instances whose replicas go (the rest is the
// controller's pick: not yet ready first, then the newest). A multi-node
// deployment lists every replica it wants, each a leader node and its
// follower nodes in rank order; a replica left out drains and goes. PD scales
// its roles with PUT /deployments/{id}/pd instead.
type replicasRequest struct {
	Replicas *int              `json:"replicas,omitempty"`
	Remove   []string          `json:"remove,omitempty"`
	Units    []planning.PDUnit `json:"units,omitempty"`
}

func (a *apiHandlers) scaleReplicas(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req replicasRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if pd != nil {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a PD deployment scales its roles with PUT /deployments/{id}/pd", map[string]any{"want": "pd"})
		return
	}
	if dep.State == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	if dep.NNodes > 1 {
		if req.Replicas != nil || len(req.Remove) > 0 || len(req.Units) == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a multi-node deployment's replicas are listed as units", map[string]any{"want": "units"})
			return
		}
		a.setReplicaUnits(w, dep, req.Units)
		return
	}
	if len(req.Units) > 0 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a single-node deployment's replicas are a count", map[string]any{"want": "replicas"})
		return
	}
	if req.Replicas == nil || *req.Replicas < 1 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasTooFew, "replicas must be at least 1; stop the deployment to run none", map[string]any{"minimum": 1})
		return
	}
	replicas := *req.Replicas
	if replicas > planning.MaxReplicas {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasTooMany, fmt.Sprintf("replicas must be at most %d", planning.MaxReplicas), map[string]any{"maximum": planning.MaxReplicas})
		return
	}
	current := max(dep.Replicas, 1)
	if shrink := current - replicas; len(req.Remove) > max(shrink, 0) {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(req.Remove), "maximum": max(shrink, 0)})
		return
	}
	if len(req.Remove) > 0 {
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
		for i, uuid := range req.Remove {
			slot := match.SlotOf(uuid)
			if slot == nil || slot.Inst.UUID != uuid || slices.Contains(req.Remove[:i], uuid) {
				writeProductError(w, http.StatusBadRequest, ErrorReplicasRemoveInvalid, "a named instance does not fill a replica of this deployment", map[string]any{"instance": uuid})
				return
			}
		}
	}
	err = a.d.Store.ScaleDeploymentReplicas(dep.ID, dep.Replicas, replicas, req.Remove)
	if errors.Is(err, store.ErrStateChanged) {
		writeProductError(w, http.StatusConflict, ErrorReplicasChanged, "the replicas changed since the request was read; read them again", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		// Deleted while the request was read.
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
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

// setReplicaUnits writes a multi-node deployment's listed replicas, as
// planning.PlanReplicaUnits checks and derives them.
func (a *apiHandlers) setReplicaUnits(w http.ResponseWriter, dep *store.Deployment, units []planning.PDUnit) {
	nodes := map[string]*store.Node{}
	var named []*store.Node
	for _, uuid := range planning.ReplicaUnitNodes(units) {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // PlanReplicaUnits names the unknown node
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		nodes[uuid] = node
		named = append(named, node)
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	patch, err := planning.PlanReplicaUnits(dep, reqs, a.resolveForRequirements(named, reqs), units, nodes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.d.Store.SetDeploymentReplicaUnits(dep.ID, patch); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			// Deleted while the request was read.
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}

// replicaUnitsDTO is a multi-node deployment's replicas as the response lists
// them: each one's leader and follower nodes, named, with its spent retry
// budget; the shape a PD role's units take.
func replicaUnitsDTO(rows dtoLookup, d *store.Deployment, view replicaView) []dto.DeploymentPDUnit {
	replicas := planning.ReplicaUnitsOf(d, view.ordered)
	out := make([]dto.DeploymentPDUnit, 0, len(replicas))
	for index, unit := range replicas {
		item := dto.DeploymentPDUnit{NodeUUID: unit.NodeUUID, NodeName: view.names[unit.NodeUUID]}
		if index < len(view.match.Slots) {
			slot := view.match.Slots[index]
			unitAttempts(rows, &item, slot, view.match.Failed[slot.Key])
		}
		for _, rank := range unit.Followers {
			item.Followers = append(item.Followers, dto.DeploymentPDRank{NodeUUID: rank.NodeUUID, NodeName: view.names[rank.NodeUUID]})
		}
		out = append(out, item)
	}
	return out
}
