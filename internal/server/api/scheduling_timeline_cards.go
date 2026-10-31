package api

import (
	"slices"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// placements is where each deployment's live instances are now: deployment
// id to node id to the GPU indexes its instances hold there.
type placements map[uint]map[uint][]int

// placementsOf gathers the live instances' cards from the instances the
// timeline already read; removed instances have no place now.
func placementsOf(insts []store.ResidencyInstance) placements {
	out := placements{}
	for _, inst := range insts {
		if !inst.Live || inst.NodeID == 0 {
			continue
		}
		byNode := out[inst.DeploymentID]
		if byNode == nil {
			byNode = map[uint][]int{}
			out[inst.DeploymentID] = byNode
		}
		for _, index := range inst.GPUs {
			if !slices.Contains(byNode[inst.NodeID], index) {
				byNode[inst.NodeID] = append(byNode[inst.NodeID], index)
			}
		}
		if byNode[inst.NodeID] == nil {
			byNode[inst.NodeID] = []int{}
		}
	}
	return out
}

// cardsOf names where each of ids is now, one read of the names of the nodes
// involved; a node no longer known keeps its id with no name.
func (a *apiHandlers) cardsOf(placed placements, ids []uint) (map[uint][]dto.TimelineCards, error) {
	nodeIDs := map[uint]bool{}
	for _, id := range ids {
		for nodeID := range placed[id] {
			nodeIDs[nodeID] = true
		}
	}
	out := map[uint][]dto.TimelineCards{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	list := make([]uint, 0, len(nodeIDs))
	for nodeID := range nodeIDs {
		list = append(list, nodeID)
	}
	names, err := a.d.Store.NodeNamesByID(list)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		for nodeID, gpus := range placed[id] {
			sorted := slices.Clone(gpus)
			sort.Ints(sorted)
			out[id] = append(out[id], dto.TimelineCards{NodeID: nodeID, Node: names[nodeID], GPUs: sorted})
		}
		sort.Slice(out[id], func(i, j int) bool {
			if out[id][i].Node != out[id][j].Node {
				return out[id][i].Node < out[id][j].Node
			}
			return out[id][i].NodeID < out[id][j].NodeID
		})
	}
	return out, nil
}

func nonNilCards(cards []dto.TimelineCards) []dto.TimelineCards {
	if cards == nil {
		return []dto.TimelineCards{}
	}
	return cards
}
