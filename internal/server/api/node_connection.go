package api

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeConnectionView changes only a copy used for presentation. The live
// control session is authoritative; stored state is a compatibility fallback
// for callers assembled without a hub, such as an offline fixture.
func (a *apiHandlers) nodeConnectionView(n store.Node) store.Node {
	if a.d.Online != nil {
		n.State = n.ConnectionStateForSession(a.d.Online(n.UUID))
	}
	return n
}

// nodeConnectionViews takes one presentation snapshot before a list is
// filtered, sorted or counted. Never pass these copies to a storage write.
func (a *apiHandlers) nodeConnectionViews(nodes []store.Node) []store.Node {
	if a.d.Online == nil {
		return nodes
	}
	views := slices.Clone(nodes)
	for i := range views {
		views[i] = a.nodeConnectionView(views[i])
	}
	return views
}
