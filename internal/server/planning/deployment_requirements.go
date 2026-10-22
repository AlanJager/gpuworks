package planning

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RequirementsRequest is what a create or evaluate request says about where a
// deployment may run, in the request's own terms.
type RequirementsRequest struct {
	// Stated is node_requirements as sent.
	Stated map[string][]string
	// TargetVendor is target_vendor as sent; it becomes the vendor
	// requirement.
	TargetVendor string
	// ManualNode is the node a manually placed single-node request names, ""
	// for an automatic one. A manual placement is a node requirement naming
	// it (nodelabel.KeyNode).
	ManualNode string
	// PerRank is a multi-node or PD request, which names its nodes rank by
	// rank; a node requirement does not apply to it.
	PerRank bool
}

// RequestRequirements turns a request into the requirements a deployment
// stores, normalized. It does not consult any node: a vendor the request
// leaves unstated is filled from the node the deployment lands on, once that
// is known (WithNodeVendor).
func RequestRequirements(r RequirementsRequest) (nodelabel.Requirements, error) {
	stated := nodelabel.Requirements{}
	for key, values := range r.Stated {
		stated[key] = append([]string(nil), values...)
	}
	if r.TargetVendor != "" {
		vendor := accelerator.NormalizeVendor(r.TargetVendor)
		if vendor == "" {
			return nil, invalidRequirements("target_vendor " + r.TargetVendor + " is not a supported vendor")
		}
		if named, ok := stated[nodelabel.KeyVendor]; ok && !(len(named) > 0 && allEqual(named, vendor)) {
			return nil, invalidRequirements("target_vendor " + vendor + " disagrees with the stated " + nodelabel.KeyVendor + " requirement")
		}
		stated[nodelabel.KeyVendor] = []string{vendor}
	}
	if _, named := stated[nodelabel.KeyNode]; named && r.PerRank {
		return nil, invalidRequirements("a multi-node or PD deployment names its nodes per rank, not as a " + nodelabel.KeyNode + " requirement")
	}
	if r.ManualNode != "" {
		if named, ok := stated[nodelabel.KeyNode]; ok {
			if !contains(named, r.ManualNode) {
				return nil, invalidRequirements("node " + r.ManualNode + " is not among the stated " + nodelabel.KeyNode + " requirement")
			}
		} else {
			stated[nodelabel.KeyNode] = []string{r.ManualNode}
		}
	}
	normalized, err := nodelabel.NormalizeRequirements(stated)
	if err != nil {
		return nil, invalidRequirements(err.Error())
	}
	return normalized, nil
}

// EditedRequirements are the requirements a stored single-node deployment
// takes from an edit, normalized. The vendor is the one thing an edit cannot
// change: the deployment carries one image, built for that vendor. An edit
// that leaves the vendor unstated keeps it.
func EditedRequirements(stated, stored nodelabel.Requirements) (nodelabel.Requirements, error) {
	vendor := RequirementVendor(stored)
	if named, ok := stated[nodelabel.KeyVendor]; ok && vendor != "" && !(len(named) > 0 && allEqual(named, vendor)) {
		return nil, invalidRequirements("the deployment's image is for " + vendor + "; " + nodelabel.KeyVendor + " cannot change - deploy again for another vendor")
	}
	return RequestRequirements(RequirementsRequest{Stated: stated, TargetVendor: vendor})
}

// PlacementStrategyOf reads placement_strategy: unstated is the default.
func PlacementStrategyOf(stated string) (store.PlacementStrategy, error) {
	if stated == "" {
		return store.PlacementSpread, nil
	}
	for _, strategy := range store.PlacementStrategies() {
		if string(strategy) == stated {
			return strategy, nil
		}
	}
	return "", messages.NewProductError(messages.ErrorPlacementStrategyInvalid, "placement_strategy must be spread or binpack", map[string]any{"strategy": stated})
}

// WithNodeVendor fills the vendor requirement from the node a deployment
// lands on when the request did not state one: a deployment carries one image,
// built for one vendor, so its vendor is a fact from the moment it is
// created. A node that reports no recognized vendor leaves it unstated.
func WithNodeVendor(reqs nodelabel.Requirements, node *store.Node) nodelabel.Requirements {
	if len(reqs[nodelabel.KeyVendor]) > 0 || node == nil {
		return reqs
	}
	vendor := accelerator.NormalizeVendor(node.GPUVendor)
	if vendor == "" {
		return reqs
	}
	out := make(nodelabel.Requirements, len(reqs)+1)
	for key, values := range reqs {
		out[key] = values
	}
	out[nodelabel.KeyVendor] = []string{vendor}
	return out
}

// RequirementVendor is the vendor requirements name, "" when they name none.
func RequirementVendor(reqs nodelabel.Requirements) string {
	if vendors := reqs[nodelabel.KeyVendor]; len(vendors) == 1 {
		return vendors[0]
	}
	return ""
}

// IsVendorRequirement reports whether an unmet requirement is the vendor
// one, which the deploy form explains as a vendor mismatch.
func IsVendorRequirement(unmet *UnmetRequirement) bool {
	return unmet != nil && unmet.Key == nodelabel.KeyVendor
}

// PinnedNode is the one node requirements pin a deployment to, when they
// name exactly one.
func PinnedNode(reqs nodelabel.Requirements) (string, bool) {
	if nodes := reqs[nodelabel.KeyNode]; len(nodes) == 1 {
		return nodes[0], true
	}
	return "", false
}

// NamedNodes is the nodes requirements name (their node requirement), nil
// when they name none. No other node can satisfy them.
func NamedNodes(reqs nodelabel.Requirements) []string {
	return reqs[nodelabel.KeyNode]
}

// OnlyNamesNodes reports whether the node requirement is all reqs state: what
// they say about any node then follows from the named nodes alone.
func OnlyNamesNodes(reqs nodelabel.Requirements) bool {
	return len(reqs) == 1 && len(reqs[nodelabel.KeyNode]) > 0
}

// IsManualPlacement reports whether requirements pin a deployment to named
// nodes. That is all "manual" means; nothing else records it.
func IsManualPlacement(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyNode]) > 0
}

// UnmetRequirementsError is the product error for a node a request names that
// its requirements exclude.
func UnmetRequirementsError(err error) error {
	return messages.NewProductError(messages.ErrorNodeRequirementsUnmet, err.Error(), map[string]any{"detail": err.Error()})
}

func invalidRequirements(detail string) error {
	return messages.NewProductError(messages.ErrorNodeRequirementsInvalid, detail, map[string]any{"detail": detail})
}

func allEqual(values []string, want string) bool {
	for _, value := range values {
		if value != want {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
