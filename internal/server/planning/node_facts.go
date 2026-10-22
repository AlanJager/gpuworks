package planning

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// NodeFacts are what generating a node's system labels needs beyond the node
// row. Either part may be nil, which leaves its labels off - that is what a
// caller that did not load it knows, not a claim that the node has none.
type NodeFacts struct {
	// Networks is the nodes' network resolution.
	Networks netplan.Resolution
	// Storages names the storages that apply to each node, by node id.
	Storages StorageScope
	// Providers says where each rented node runs, by node id.
	Providers map[uint]store.NodeProvider
}

// StorageScope lists the storages that apply to each node, by node id.
type StorageScope map[uint][]store.Storage

// LoadNodeFacts loads what matching reqs against nodes reads beyond the node
// rows: their networks (through resolve, the caller's network resolution) and
// the storages that apply to them, each only when a requirement names one. A
// part that cannot be loaded stays nil, which leaves its requirements unmet:
// an unreadable fact is never a match.
func LoadNodeFacts(st *store.Store, resolve func([]store.Node) (netplan.Resolution, error), nodes []store.Node, reqs nodelabel.Requirements) NodeFacts {
	var facts NodeFacts
	if RequirementsNeedNetworks(reqs) && resolve != nil {
		if resolved, err := resolve(nodes); err == nil {
			facts.Networks = resolved
		}
	}
	if RequirementsNeedProviders(reqs) {
		ids := make([]uint, len(nodes))
		for i := range nodes {
			ids[i] = nodes[i].ID
		}
		if providers, err := st.NodeProviders(ids); err == nil {
			facts.Providers = providers
		}
	}
	if RequirementsNeedStorages(reqs) {
		ptrs := make([]*store.Node, len(nodes))
		for i := range nodes {
			ptrs[i] = &nodes[i]
		}
		if storages, err := LoadStorageScope(st, ptrs); err == nil {
			facts.Storages = storages
		}
	}
	return facts
}

// LoadStorageScope reads which storages apply to each of nodes.
func LoadStorageScope(st *store.Store, nodes []*store.Node) (StorageScope, error) {
	byNode, err := st.StorageScopes(nodes)
	if err != nil {
		return nil, err
	}
	return StorageScope(byNode), nil
}

// RequirementsNeedProviders reports whether matching reqs reads where rented
// nodes run.
func RequirementsNeedProviders(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyProvider]) > 0 || len(reqs[nodelabel.KeyRegion]) > 0
}

// RequirementsNeedStorages reports whether matching reqs reads which
// storages apply to a node.
func RequirementsNeedStorages(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyStorage]) > 0
}

// storageLabels names every storage that applies to the node: whether it is
// mounted right now is not a label but a reason admission gives.
func storageLabels(node *store.Node, scope StorageScope) []SystemLabel {
	if scope == nil {
		return nil
	}
	var labels []SystemLabel
	for _, storage := range scope[node.ID] {
		labels = append(labels, SystemLabel{
			Key: nodelabel.KeyStorage, Value: strconv.FormatUint(uint64(storage.ID), 10), Display: storage.Name,
		})
	}
	return labels
}
