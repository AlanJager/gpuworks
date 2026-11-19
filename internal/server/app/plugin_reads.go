package app

import (
	"context"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pluginReads answers the plugin host API's node reads from the store's
// pinned reads (store.NodeIdentity, store.NodeInventory): each call is the
// store's two indexed queries for at most 500 nodes, never a fleet scan.
type pluginReads struct{ st *store.Store }

// PluginReads is the plugin host's reads of core facts, served by st.
func PluginReads(st *store.Store) pluginhost.Reads { return pluginReads{st} }

func (r pluginReads) NodeIdentity(ctx context.Context, ids []uint64, key []byte) ([]extapi.NodeIdentity, error) {
	nodeIDs := make([]uint, len(ids))
	for i, id := range ids {
		nodeIDs[i] = uint(id)
	}
	rows, err := r.st.NodeIdentity(ctx, nodeIDs, key)
	if err != nil {
		return nil, err
	}
	out := make([]extapi.NodeIdentity, len(rows))
	for i, row := range rows {
		n := extapi.NodeIdentity{NodeID: uint64(row.NodeID), CardsWithoutSerial: row.CardsWithoutSerial,
			Cards: make([]extapi.IdentityPart, len(row.Cards))}
		if row.Host.Known() {
			n.Host = &extapi.IdentityPart{H: row.Host.H, Tail: row.Host.Tail}
		}
		for j, c := range row.Cards {
			n.Cards[j] = extapi.IdentityPart{H: c.H, Tail: c.Tail}
		}
		out[i] = n
	}
	return out, nil
}

func (r pluginReads) NodeInventory(ctx context.Context, cursor uint64, limit int) ([]extapi.InventoryNode, uint64, error) {
	page, err := r.st.NodeInventory(ctx, uint(cursor), limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]extapi.InventoryNode, len(page.Nodes))
	for i, n := range page.Nodes {
		node := extapi.InventoryNode{ID: uint64(n.ID), UUID: n.UUID, Name: clipText(n.Name, extapi.MaxNodeText),
			EnrolledAt: extapi.NewTime(n.CreatedAt), HostReported: n.HostReported, Cards: make([]extapi.InventoryCard, len(n.Cards))}
		if node.Name == "" {
			node.Name = clipText(n.UUID, extapi.MaxNodeText)
		}
		for j, c := range n.Cards {
			node.Cards[j] = extapi.InventoryCard{Index: max(c.Index, -1), Model: clipText(c.Model, extapi.MaxNodeText), Serial: c.Serial}
		}
		out[i] = node
	}
	return out, uint64(page.Next), nil
}

// clipText cuts s to at most n bytes at a rune boundary, so a long name
// set by an operator still fits the wire's bound.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
