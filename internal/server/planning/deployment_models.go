package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentModelIDs is the common dependency set for admission, launch and
// preparation display: the deployment's own model, every other rank's model
// when its weights are rank-scoped, each template component's, then each
// model an extra mount row names. Component and rank bindings are immutable
// model IDs.
func DeploymentModelIDs(dep *store.Deployment) ([]uint, error) {
	return deploymentModelIDs(dep, -1)
}

// RankModelIDs is what one rank of the deployment reads: the model its
// primary mount takes first (the deployment's own, or the rank's own artifact
// for rank-scoped weights, ADR 0076), then the models every rank shares.
func RankModelIDs(dep *store.Deployment, rank int) ([]uint, error) {
	return deploymentModelIDs(dep, max(rank, 0))
}

// deploymentModelIDs lists rank's models, or every rank's for a negative rank.
func deploymentModelIDs(dep *store.Deployment, rank int) ([]uint, error) {
	layer, err := DeploymentTemplate(dep)
	if err != nil {
		return nil, err
	}
	primary := dep.ModelID
	if id, ok := layer.RankWeightModelID(rank); ok {
		primary = id
	}
	ids := []uint{primary}
	seen := map[uint]bool{primary: true}
	add := func(id uint) {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if rank < 0 {
		for _, weight := range layer.RankWeights {
			if weight.StoreModelID == 0 {
				return nil, fmt.Errorf("deployment %d rank %d has no immutable model binding", dep.ID, weight.Rank)
			}
			add(weight.StoreModelID)
		}
	}
	for _, component := range layer.Components {
		if component.StoreModelID == 0 {
			return nil, fmt.Errorf("deployment %d component %s has no immutable model binding", dep.ID, component.Name)
		}
		add(component.StoreModelID)
	}
	for _, id := range LaunchSourceFromDeployment(dep).Overrides.ModelMountIDs() {
		add(id)
	}
	return ids, nil
}

// NodeModelIDs returns what each node reads of the deployment: its rank's
// models when the weights are rank-scoped and the deployment places the node
// at one rank (nodeRanks), every rank's otherwise. The snapshot is decoded
// once, however many nodes are asked about.
func NodeModelIDs(dep *store.Deployment) (func(*store.Node) []uint, error) {
	all, err := DeploymentModelIDs(dep)
	if err != nil {
		return nil, err
	}
	layer, err := DeploymentTemplate(dep)
	if err != nil {
		return nil, err
	}
	if !layer.RankScoped() {
		return func(*store.Node) []uint { return all }, nil
	}
	byUUID, byID := nodeRanks(dep)
	perRank := map[int][]uint{}
	for _, weight := range layer.RankWeights {
		if perRank[weight.Rank], err = RankModelIDs(dep, weight.Rank); err != nil {
			return nil, err
		}
	}
	return func(node *store.Node) []uint {
		if node == nil {
			return all
		}
		rank, ok := byUUID[node.UUID]
		if !ok {
			rank, ok = byID[node.ID]
		}
		if ids, bound := perRank[rank]; ok && bound {
			return ids
		}
		return all
	}, nil
}

// nodeRanks is the rank a deployment places each of its nodes at: the rank a
// listed replica names it for (by UUID), or its position in the stored node
// list (by ID).
func nodeRanks(dep *store.Deployment) (map[string]int, map[uint]int) {
	byUUID, byID := map[string]int{}, map[uint]int{}
	if placement, err := ParsePlacement(dep.Placement); err == nil {
		for _, unit := range placement.Units {
			for rank, member := range unit.Ranks() {
				if member.NodeUUID != "" {
					byUUID[member.NodeUUID] = rank
				}
			}
		}
	}
	for rank, id := range dep.NodeIDList() {
		if _, seen := byID[id]; !seen {
			byID[id] = rank
		}
	}
	return byUUID, byID
}
