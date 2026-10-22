package planning

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RequirementLabel is one value of a deployment's node requirements as a
// person reads it: shown the way the node pages show that label, with how
// many nodes carry it now. Nodes 0 is a value nothing carries - a deleted
// node or network, a label no node has - which still constrains the
// deployment (it matches nothing) and is worth saying so.
type RequirementLabel struct {
	Key   string
	Value string
	// Display is the text to show: a node's or network's name for a system
	// value some node carries, otherwise the value itself.
	Display string
	// DisplayCode names a client translation, as SystemLabel.DisplayCode.
	DisplayCode string
	Nodes       int
}

// RequirementLabels describes every value of reqs against nodes, in key then
// value order. facts are the nodes' networks and storages, each nil when reqs
// name none (RequirementsNeedNetworks, RequirementsNeedStorages).
func RequirementLabels(reqs nodelabel.Requirements, nodes []*store.Node, facts NodeFacts) []RequirementLabel {
	if len(reqs) == 0 {
		return nil
	}
	type pair struct{ key, value string }
	wanted := map[pair]*RequirementLabel{}
	var out []*RequirementLabel
	for key, values := range reqs {
		for _, value := range values {
			label := &RequirementLabel{Key: key, Value: value, Display: value}
			wanted[pair{key, value}] = label
			out = append(out, label)
		}
	}
	for _, node := range nodes {
		seen := map[pair]bool{}
		for _, system := range SystemLabels(node, facts) {
			p := pair{system.Key, system.Value}
			label := wanted[p]
			if label == nil || seen[p] {
				continue
			}
			seen[p] = true
			label.Nodes++
			label.Display, label.DisplayCode = system.Display, system.DisplayCode
		}
		written, err := store.NodeLabels(node.Labels)
		if err != nil {
			continue // fails closed, as MatchRequirements
		}
		for key, value := range written {
			if nodelabel.IsSystemKey(key) {
				continue
			}
			if label := wanted[pair{key, value}]; label != nil {
				label.Nodes++
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return labelBefore(out[i].Key, out[i].Value, out[j].Key, out[j].Value)
	})
	labels := make([]RequirementLabel, len(out))
	for i, label := range out {
		labels[i] = *label
	}
	return labels
}
