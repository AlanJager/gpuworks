package app

import (
	"context"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/hwident"
	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pluginReads answers the plugin host API's node reads from the store's
// pinned reads (store.NodeIdentity, store.NodeInventory): each call is the
// store's two indexed queries for at most 500 nodes, never a fleet scan.
// The host identity is the server's own facts, read once per process.
type pluginReads struct {
	st   *store.Store
	host func() hostidentity.Facts
}

// PluginReads is the plugin host's reads of core facts, served by st and
// this machine's host identity (hostidentity.Local).
func PluginReads(st *store.Store) pluginhost.Reads {
	return pluginReads{st: st, host: hostidentity.Local}
}

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
		n.Host = wirePart(row.Host)
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

func (r pluginReads) HostIdentity(_ context.Context, key []byte) (extapi.HostIdentityAnswer, error) {
	s := r.host().Salted(key)
	return extapi.HostIdentityAnswer{SystemUUID: wirePart(s.SystemUUID), BoardSerial: wirePart(s.BoardSerial),
		MachineID: wirePart(s.MachineID), RootFSUUID: wirePart(s.RootFSUUID)}, nil
}

// wirePart is a known part on the wire, nil for an unknown one.
func wirePart(p hwident.IdentityPart) *extapi.IdentityPart {
	if !p.Known() {
		return nil
	}
	return &extapi.IdentityPart{H: p.H, Tail: p.Tail}
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
