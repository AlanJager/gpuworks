package planning

import (
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// UnmetRequirement is the first of a deployment's node requirements, in key
// order, that a node does not satisfy. Its error text names the node, the key
// and the values that would have satisfied it.
type UnmetRequirement struct {
	Node   string
	Key    string
	Values []string
}

func (u *UnmetRequirement) Error() string {
	return fmt.Sprintf("node %s does not satisfy %s ∈ {%s}", u.Node, u.Key, strings.Join(u.Values, ", "))
}

// RequirementsNeedNetworks reports whether matching reqs reads a node's
// resolved networks, so a caller can skip resolving them when it does not.
func RequirementsNeedNetworks(reqs nodelabel.Requirements) bool {
	for _, key := range []string{nodelabel.KeyComputeNetwork, nodelabel.KeyStorageSyncNetwork, nodelabel.KeyServingNetwork} {
		if len(reqs[key]) > 0 {
			return true
		}
	}
	return false
}

// RequirementsNeedCards reports whether matching reqs reads a node's cards
// (the GPU model and architecture labels).
func RequirementsNeedCards(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyGPUModel]) > 0 || len(reqs[nodelabel.KeyGPUArch]) > 0
}

// MatchRequirements checks a node against a deployment's node requirements:
// every key must hold, and a key holds when the node carries any of its
// values. A system key is matched against the labels the platform generates
// for the node (SystemLabels, so the node page and this check can never
// disagree), any other key against the node's written labels.
//
// It fails closed. A node whose written labels do not decode satisfies no
// written-label key, and a network or storage key is unmet when facts do not
// cover the node: an unreadable fact is never read as a match. A key the stored
// requirements name that no node can carry - a deleted node's UUID, a key no
// longer generated - simply matches nothing.
//
// It returns nil when the node satisfies every requirement, an
// *UnmetRequirement when it does not, and a plain error for a missing node or
// unreadable labels.
func MatchRequirements(node *store.Node, facts NodeFacts, reqs nodelabel.Requirements) error {
	if len(reqs) == 0 {
		return nil
	}
	if node == nil {
		return fmt.Errorf("node requirements need a node to check")
	}
	keys := make([]string, 0, len(reqs))
	for key := range reqs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var system []SystemLabel
	var written map[string]string
	for _, key := range keys {
		var have []string
		if nodelabel.IsSystemKey(key) {
			if system == nil {
				system = SystemLabels(node, facts)
			}
			have = SystemLabelValues(system, key)
		} else {
			if written == nil {
				labels, err := store.NodeLabels(node.Labels)
				if err != nil {
					return fmt.Errorf("node %s labels: %w", node.UUID, err)
				}
				written = labels
			}
			if value, ok := written[key]; ok {
				have = []string{value}
			}
		}
		if !anyOf(have, reqs[key]) {
			return &UnmetRequirement{Node: node.DisplayName(), Key: key, Values: reqs[key]}
		}
	}
	return nil
}

func anyOf(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}
