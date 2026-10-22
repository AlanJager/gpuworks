package store

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// parseTopoJSON parses accelerator topology output and returns the serialized
// topoparse.Topology (empty on failure — best-effort).
func parseTopoJSON(raw string, reported ...[]proto.GPUInfo) string {
	topo, err := topoparse.ParseAuto(raw)
	if err != nil || topo == nil {
		return ""
	}
	if len(reported) > 0 {
		numaByIndex := make(map[int]int, len(reported[0]))
		for _, g := range reported[0] {
			numaByIndex[g.Index] = g.NUMANode
		}
		topo.NUMAGroups = map[int][]int{}
		for i := range topo.GPUs {
			if numa, ok := numaByIndex[topo.GPUs[i].Index]; ok {
				topo.GPUs[i].NUMANode = numa
			}
			if topo.GPUs[i].NUMANode >= 0 {
				topo.NUMAGroups[topo.GPUs[i].NUMANode] = append(topo.NUMAGroups[topo.GPUs[i].NUMANode], topo.GPUs[i].Index)
			}
		}
	}
	b, err := json.Marshal(topo)
	if err != nil {
		return ""
	}
	return string(b)
}

// NodeTopology returns the parsed topology for a node, or nil if none stored.
func (s *Store) NodeTopology(nodeID uint) *topoparse.Topology {
	var n Node
	if err := s.db.Select("topo_json").First(&n, nodeID).Error; err != nil {
		return nil
	}
	if n.TopoJSON == "" {
		return nil
	}
	var t topoparse.Topology
	if err := json.Unmarshal([]byte(n.TopoJSON), &t); err != nil {
		return nil
	}
	return &t
}
