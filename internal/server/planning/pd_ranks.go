package planning

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// Ranks is the unit's ranks in rank order: the leader (rank 0) and then its
// followers. A single-node unit has one.
func (u PDUnit) Ranks() []PDRank {
	out := make([]PDRank, 0, 1+len(u.Followers))
	out = append(out, PDRank{NodeUUID: u.NodeUUID, GPUIndexes: u.GPUIndexes})
	return append(out, u.Followers...)
}

// MultiNode reports whether the unit spans nodes.
func (u PDUnit) MultiNode() bool { return len(u.Followers) > 0 }

// Parallelism is what one unit of the role runs: the role's TP and PP over
// the unit's ranks.
func (p *PDPlacement) Parallelism(role string, unit PDUnit) parallelism.Spec {
	return parallelism.Spec{TP: p.TP(role), PP: p.Roles[role].PP, NNodes: len(unit.Ranks())}.Normalized()
}

// RankGPUs is how many GPUs each rank of the unit runs; checkUnitRanks has
// made sure the ranks share the role's TP×PP equally.
func (p *PDPlacement) RankGPUs(role string, unit PDUnit) int {
	share, err := p.Parallelism(role, unit).GPUsPerNode()
	if err != nil || share < 1 {
		return 1
	}
	return share
}

// checkUnitRanks checks a unit's rank list: every rank names a node, no node
// carries two ranks of one unit (a rank is one process per node, as for any
// multi-node group), and a multi-node unit states the role's TP and splits
// TP×PP equally over its ranks.
func (p *PDPlacement) checkUnitRanks(role string, unit PDUnit) error {
	seen := map[string]bool{}
	for _, rank := range unit.Ranks() {
		if rank.NodeUUID == "" {
			return pdError(messages.ErrorPDNodeRequired, fmt.Sprintf("an instance of the %s role names no node", role), map[string]any{"role": role})
		}
		if seen[rank.NodeUUID] {
			return pdError(messages.ErrorPDRankNodeRepeated, fmt.Sprintf("an instance of the %s role places two ranks on %s", role, rank.NodeUUID), map[string]any{"role": role, "node": rank.NodeUUID})
		}
		seen[rank.NodeUUID] = true
	}
	if !unit.MultiNode() {
		return nil
	}
	shape := p.Roles[role]
	spec := parallelism.Spec{TP: shape.TP, PP: shape.PP, NNodes: len(unit.Ranks())}
	if _, err := spec.GPUsPerNode(); err != nil {
		code := messages.ErrorPDRankSplit
		if errors.Is(err, parallelism.ErrTPRequired) {
			code = messages.ErrorPDRankTPRequired
		}
		normalized := spec.Normalized()
		return pdError(code, fmt.Sprintf("the %s role: %v", role, err), map[string]any{"role": role, "tp": normalized.TP, "pp": normalized.PP, "ranks": normalized.NNodes})
	}
	return nil
}
