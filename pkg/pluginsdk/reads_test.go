package pluginsdk_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// startReader starts a plugin that uses the node reads, initialized with
// the permissions perms, and returns it with its harness.
func startReader(t *testing.T, perms ...string) (*plugintest.Harness, *pluginsdk.Plugin) {
	t.Helper()
	cfg := baseConfig()
	cfg.Uses = map[string]extapi.Range{extapi.CapHostNodeIdentity: {Min: 1, Max: 1}, extapi.CapHostNodes: {Min: 1, Max: 1}}
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	req := h.Request()
	req.Plugin.PermissionsGranted = perms
	if _, err := h.Initialize(req); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-ready:
		return h, p
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin did not start")
		return nil, nil
	}
}

func inventoryNode(id uint64) extapi.InventoryNode {
	return extapi.InventoryNode{ID: id, UUID: fmt.Sprintf("uuid-%d", id), Name: fmt.Sprintf("gpu-%02d", id),
		EnrolledAt: extapi.NewTime(time.Date(2026, 11, 1, 0, 0, int(id), 0, time.UTC)), HostReported: id%2 == 0,
		Cards: []extapi.InventoryCard{{Index: 0, Model: "NVIDIA H800", Serial: true}}}
}

// A plugin pages through the inventory by following Next, and reads the
// identity of the nodes it shows.
func TestPluginReadsNodesAndIdentity(t *testing.T) {
	h, p := startReader(t, "read:node_identity", "read:node_inventory")
	h.SetNodes(inventoryNode(9), inventoryNode(2), inventoryNode(5))
	h.SetNodeIdentities(extapi.NodeIdentity{NodeID: 5, Host: &extapi.IdentityPart{H: "0123456789abcdef", Tail: "3432"},
		Cards: []extapi.IdentityPart{}, CardsWithoutSerial: 1})
	ctx := context.Background()

	var seen []uint64
	q := extapi.NodesQuery{Limit: 2}
	for pages := 0; ; pages++ {
		page, err := p.Nodes(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page.Nodes {
			seen = append(seen, n.ID)
		}
		if page.Next == "" {
			break
		}
		if pages > 3 {
			t.Fatal("paging does not end")
		}
		q.Cursor = page.Next
	}
	if fmt.Sprint(seen) != "[2 5 9]" {
		t.Fatalf("paged %v", seen)
	}

	ans, err := p.NodeIdentity(ctx, "acme-2026", []uint64{9, 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Nodes) != 1 || ans.Nodes[0].NodeID != 5 || ans.Nodes[0].Host.Tail != "3432" {
		t.Fatalf("identity = %+v", ans)
	}
}

// The server's refusals reach the plugin as *Error with their codes: a
// permission the administrator did not grant, and arguments out of bounds.
// A capability the plugin did not negotiate fails before any call.
func TestPluginReadsRefusals(t *testing.T) {
	h, p := startReader(t, "read:node_inventory")
	h.SetNodes(inventoryNode(1))
	ctx := context.Background()
	_, err := p.NodeIdentity(ctx, "s", []uint64{1})
	if e := wireErr(t, err); e.Status != 403 || e.Wire.Code != "PLUGIN_HOST_NOT_PERMITTED" || e.Wire.Params["permission"] != "read:node_identity" {
		t.Fatalf("identity without permission = %+v", e)
	}
	_, err = p.Nodes(ctx, extapi.NodesQuery{Cursor: "x"})
	if e := wireErr(t, err); e.Status != 400 || e.Wire.Code != "PLUGIN_HOST_ARGUMENT_INVALID" || e.Wire.Params["field"] != "cursor" {
		t.Fatalf("bad cursor = %+v", e)
	}
	_, err = p.Nodes(ctx, extapi.NodesQuery{Limit: extapi.MaxNodeRead + 1})
	if e := wireErr(t, err); e.Wire.Code != "PLUGIN_HOST_ARGUMENT_INVALID" || e.Wire.Params["field"] != "limit" {
		t.Fatalf("bad limit = %+v", e)
	}

	cfg := baseConfig()
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h2 := plugintest.Start(t, cfg)
	req := h2.Request()
	req.Plugin.PermissionsGranted = []string{"read:node_inventory"}
	if _, err := h2.Initialize(req); err != nil {
		t.Fatal(err)
	}
	if _, err := (<-ready).Nodes(ctx, extapi.NodesQuery{}); !errors.Is(err, pluginsdk.ErrNotNegotiated) {
		t.Fatalf("read without the capability = %v", err)
	}
}
