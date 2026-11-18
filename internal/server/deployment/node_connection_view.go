package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/store"
	"time"
)

// NewListsWithConnections uses authenticated session facts for presentation.
// Connection changes rebuild the existing snapshot; unchanged polling keeps
// the same memoized deployment states and the existing refresh deadline.
func NewListsWithConnections(st *store.Store, prep ModelPreparation, now func() time.Time, online func(string) bool) *Lists {
	lists := NewLists(st, prep, now)
	lists.online = online
	return lists
}

// projectNodeConnections only changes the caller's freshly read identities,
// before they are compared with or included in a cached presentation snapshot.
// These node copies never reach a store write or a scheduling input.
func projectNodeConnections(nodes []store.Node, online func(string) bool) {
	if online == nil {
		return
	}
	for i := range nodes {
		nodes[i].State = nodes[i].ConnectionStateForSession(online(nodes[i].UUID))
	}
}

// NewStoreRowsWithConnections reads a single-object presentation with the
// same session authority as the shared lists, without additional queries.
func NewStoreRowsWithConnections(st *store.Store, prep ModelPreparation, online func(string) bool) StoreRows {
	rows := NewStoreRows(st, prep)
	rows.online = online
	return rows
}
