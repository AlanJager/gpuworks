package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Scaling a multi-node or PD deployment the platform places: the request
// says how many replicas (or units) and may name nodes for the first
// placement of the new ones; the server picks the rest the way a create does
// (deployments_auto_units.go). A named node that cannot take its rank now is
// refused, as a single-node replica's is.

// autoReplicaUnits are a multi-node deployment's replicas grown or shrunk to
// replicas. placeOn names nodes for the new replicas' ranks in order - the
// first new replica's ranks, then the next's - and remove names instances
// whose replicas go first when it shrinks.
func (a *apiHandlers) autoReplicaUnits(dep *store.Deployment, replicas int, placeOn, remove []string) ([]planning.PDUnit, int, error) {
	order, err := a.deploymentNodeUUIDs(dep)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	current := planning.ReplicaUnitsOf(dep, order)
	added := replicas - len(current)
	if len(placeOn) > max(added, 0)*dep.NNodes {
		return nil, http.StatusBadRequest, productError(ErrorReplicasPlaceOnTooMany, "more nodes named than the new replicas have ranks", map[string]any{"count": len(placeOn), "maximum": max(added, 0) * dep.NNodes})
	}
	if added <= 0 {
		return a.shrinkReplicaUnits(dep, current, replicas, remove)
	}
	if len(remove) > 0 {
		// As for a single-node deployment: instances are named only for the
		// replicas a shrink drops.
		return nil, http.StatusBadRequest, productError(ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(remove), "maximum": 0})
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	perRank, err := placement.Parallelism(dep.NNodes).GPUsPerNode()
	if err != nil || perRank < 1 {
		perRank = 1
	}
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	units := append([]planning.PDUnit(nil), current...)
	for k := 0; k < added; k++ {
		var listed []planning.ListedRank
		for _, unit := range units {
			for i, rank := range unit.Ranks() {
				listed = append(listed, planning.ListedRank{NodeUUID: rank.NodeUUID, GPUs: perRank, Leader: i == 0})
			}
		}
		facts, err := planning.LoadUnitSuggestFacts(a.d.Store, a.networks().Resolve, dep, reqs, listed, a.onlineOf(nodes), dep.NNodes, perRank)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		fixed := placeOn[min(k*dep.NNodes, len(placeOn)):min((k+1)*dep.NNodes, len(placeOn))]
		chosen, counts, ok := planning.ChooseReplicaNodes(facts, dep.PlacementStrategy, fixed, func([]*store.Node) int { return -1 })
		if !ok {
			if len(fixed) > 0 {
				name := fixed[0]
				if node, err := a.d.Store.NodeByUUID(fixed[0]); err == nil {
					name = nodeDisplayName(node)
				}
				return nil, http.StatusConflict, productError(ErrorReplicasPlaceOnRefused, fmt.Sprintf("the named nodes %v cannot carry a replica now", fixed), map[string]any{"node": name, "reason": "no_group", "detail": ""})
			}
			return nil, http.StatusConflict, unitPlacementNone(counts)
		}
		unit := planning.PDUnit{NodeUUID: chosen[0].UUID}
		for _, node := range chosen[1:] {
			unit.Followers = append(unit.Followers, planning.PDRank{NodeUUID: node.UUID})
		}
		units = append(units, unit)
	}
	return units, 0, nil
}

// shrinkReplicaUnits drops replicas down to replicas: those the named
// instances lead or follow in first, then the newest.
func (a *apiHandlers) shrinkReplicaUnits(dep *store.Deployment, current []planning.PDUnit, replicas int, remove []string) ([]planning.PDUnit, int, error) {
	if replicas < 1 {
		return nil, http.StatusBadRequest, productError(ErrorReplicasTooFew, "replicas must be at least 1; stop the deployment to run none", map[string]any{"minimum": 1})
	}
	drop := map[int]bool{}
	for _, uuid := range remove {
		inst, err := a.d.Store.InstanceByUUID(uuid)
		if err != nil || inst.DeploymentID != dep.ID {
			return nil, http.StatusBadRequest, productError(ErrorReplicasRemoveInvalid, "a named instance does not fill a replica of this deployment", map[string]any{"instance": uuid})
		}
		node, err := a.d.Store.NodeByID(inst.NodeID)
		if err != nil {
			return nil, http.StatusBadRequest, productError(ErrorReplicasRemoveInvalid, "a named instance does not fill a replica of this deployment", map[string]any{"instance": uuid})
		}
		for i, unit := range current {
			for _, rank := range unit.Ranks() {
				if rank.NodeUUID == node.UUID {
					drop[i] = true
				}
			}
		}
	}
	if len(current)-len(drop) < replicas {
		return nil, http.StatusBadRequest, productError(ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(remove), "maximum": max(len(current)-replicas, 0)})
	}
	var out []planning.PDUnit
	for i, unit := range current {
		if !drop[i] {
			out = append(out, unit)
		}
	}
	return out[:replicas], 0, nil
}

// deploymentNodeUUIDs are the deployment's stored nodes in order.
func (a *apiHandlers) deploymentNodeUUIDs(dep *store.Deployment) ([]string, error) {
	var out []string
	for _, id := range dep.NodeIDList() {
		node, err := a.d.Store.NodeByID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, node.UUID)
	}
	return out, nil
}

// autoPDRoles fills the nodes of the new units a PD update leaves out, for a
// deployment the platform places: beside the units that are placed, in the
// group they are in. A named node is the new unit's first placement and is
// checked with the rest by ValidatePDNodes.
func (a *apiHandlers) autoPDRoles(dep *store.Deployment, current *planning.PDPlacement, roles map[string]planning.PDRole) (int, error) {
	mode, ok := pdmode.Lookup(current.Mode)
	if !ok {
		return 0, nil
	}
	fixed := map[string][]planning.PDUnit{}
	var needs []planning.PDRoleNeed
	open := false
	for _, role := range mode.Roles {
		r, stated := roles[role]
		if !stated {
			fixed[role] = append(fixed[role], current.Units(role)...)
			continue
		}
		need := planning.PDRoleNeed{Role: role}
		for _, unit := range r.Units {
			if unit.NodeUUID != "" {
				fixed[role] = append(fixed[role], unit)
				continue
			}
			if len(unit.Followers) > 0 {
				return http.StatusBadRequest, productError(ErrorAutoPlacementPDMultiNode, fmt.Sprintf("automatic placement takes PD units that fit one node; the %s role's units span nodes - name their nodes", role), map[string]any{"role": role})
			}
			need.Units++
			need.GPUs = current.RankGPUs(role, unit)
			open = true
		}
		if need.Units > 0 {
			needs = append(needs, need)
		}
	}
	if !open {
		return 0, nil
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	facts, err := planning.LoadUnitSuggestFacts(a.d.Store, a.networks().Resolve, dep, reqs, listedPDRanks(current, fixed), a.onlineOf(nodes), 1, 1)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	chosen, counts, ok := planning.ChoosePDUnits(facts, needs, fixed, dep.PlacementStrategy, func(map[string][]planning.PDUnit) string { return "" })
	if !ok {
		return http.StatusConflict, unitPlacementNone(counts)
	}
	for role := range roles {
		r := roles[role]
		r.Units = chosen[role]
		roles[role] = r
	}
	return 0, nil
}

// unitsPlacementMode is the deployment's placement mode as the API states
// it: "auto" or "manual" for a multi-node or PD deployment, "" for a
// single-node one (its draft says, from its node requirements).
func unitsPlacementMode(dep *store.Deployment) string {
	pd, _ := planning.PDOf(dep.Placement)
	if dep.NNodes <= 1 && pd == nil {
		return ""
	}
	if dep.UnitsAuto {
		return "auto"
	}
	return "manual"
}

// withoutPinnedGPUs is a PD placement with every unit's pinned cards
// dropped: the platform picks cards when it places the units.
func withoutPinnedGPUs(pd *planning.PDPlacement) *planning.PDPlacement {
	out := *pd
	out.Roles = map[string]planning.PDRole{}
	for name, role := range pd.Roles {
		units := slices.Clone(role.Units)
		for i := range units {
			units[i].GPUIndexes = nil
			followers := slices.Clone(units[i].Followers)
			for j := range followers {
				followers[j].GPUIndexes = nil
			}
			units[i].Followers = followers
		}
		role.Units = units
		out.Roles[name] = role
	}
	return &out
}

// setPlacementMode switches a multi-node or PD deployment between automatic
// placement and the operator's own. Nothing restarts: the units keep their
// nodes, and the mode decides what happens when one next starts. Switching
// to automatic drops the PD units' pinned cards, which the platform then
// picks.
func (a *apiHandlers) setPlacementMode(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.PlacementModeRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Mode != "auto" && req.Mode != "manual" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "mode is auto or manual", map[string]any{"field": "mode"})
		return
	}
	if unitsPlacementMode(dep) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorPlacementModeShape, "a single-node deployment states its placement in its node requirements", nil)
		return
	}
	placement := ""
	if pd, _ := planning.PDOf(dep.Placement); pd != nil && req.Mode == "auto" {
		encoded, err := planning.WithPD(dep.Placement, withoutPinnedGPUs(pd))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		placement = encoded
	}
	if err := a.d.Store.SetDeploymentUnitsAuto(dep.ID, req.Mode == "auto", placement); err != nil {
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
