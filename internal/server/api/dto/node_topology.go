package dto

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// ParseNodeTopology reads a node's stored topology (nodes.topo_json, the
// agent's topoparse.Topology) into its wire form; nil when it does not parse.
func ParseNodeTopology(raw string) *NodeTopology {
	var t topoparse.Topology
	if json.Unmarshal([]byte(raw), &t) != nil {
		return nil
	}
	out := &NodeTopology{
		GPUs:          make([]TopologyGPU, 0, len(t.GPUs)),
		Matrix:        make([][]TopologyLink, 0, len(t.Matrix)),
		FastCliques:   t.FastCliques,
		NVLinkCliques: t.NVLinkCliques,
		NUMAGroups:    t.NUMAGroups,
	}
	for _, g := range t.GPUs {
		out.GPUs = append(out.GPUs, TopologyGPU{Index: g.Index, CPUAffinity: g.CPUAffinity, NUMANode: g.NUMANode})
	}
	for _, row := range t.Matrix {
		links := make([]TopologyLink, 0, len(row))
		for _, l := range row {
			links = append(links, TopologyLink{Raw: l.Raw, Class: int(l.Class), NVLinkBonds: l.NVLinkBonds})
		}
		out.Matrix = append(out.Matrix, links)
	}
	return out
}
