package app

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const (
	readsHostUUID   = "4C4C4544-0042-3510-8051-B4C04F4E3432"
	readsCardSerial = "GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8"
)

// readsStore is a store with one node whose agent reported the machine's
// UUID and two cards: one with a hardware serial, one whose id the probe
// made up from its index.
func readsStore(t *testing.T, name string) (*store.Store, *store.Node) {
	t.Helper()
	st, err := store.Open("file:plugin_reads_"+name+"_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	n, _, err := st.CreateNodePending("gpu-a01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeHostSystemUUID(n.UUID, readsHostUUID); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{
		{Index: 0, UUID: readsCardSerial, PCIeBDF: "0000:3b:00.0", Vendor: "nvidia", Name: "NVIDIA H800", MemoryTotalBytes: 80 << 30, Healthy: true},
		{Index: 1, UUID: "ascend-1", PCIeBDF: "0000:c1:00.0", Vendor: "ascend", Name: "Ascend 910B", MemoryTotalBytes: 64 << 30, Healthy: true},
	}
	if err := st.UpdateNodeStatus(n.UUID, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
	return st, n
}

// The salt is bound to the package: two packages with the same salt get
// unrelated digests of the same machine, while one package gets equal
// digests from two servers that hold the same hardware. Tails are the raw
// values' and the same for everyone.
func TestPluginReadsBindTheSaltToThePackage(t *testing.T) {
	ctx := context.Background()
	one, n1 := readsStore(t, "one")
	two, n2 := readsStore(t, "two")
	read := func(st *store.Store, id uint, pkg string) extapi.NodeIdentity {
		t.Helper()
		got, err := PluginReads(st).NodeIdentity(ctx, []uint64{uint64(id), 9999}, pluginhost.IdentityKey(pkg, "shared-salt"))
		if err != nil {
			t.Fatal(err)
		}
		ans := extapi.NodeIdentityAnswer{Nodes: got}
		if err := ans.Validate(); err != nil || len(got) != 1 {
			t.Fatalf("answer %+v: %v", got, err)
		}
		return got[0]
	}
	quota := read(one, n1.ID, "acme_quota")
	if quota.Host == nil || quota.Host.Tail != "3432" || len(quota.Cards) != 1 || quota.Cards[0].Tail != "e7f8" || quota.CardsWithoutSerial != 1 {
		t.Fatalf("identity = %+v", quota)
	}
	other := read(one, n1.ID, "acme_backup")
	if other.Host.H == quota.Host.H || other.Cards[0].H == quota.Cards[0].H || other.Host.Tail != quota.Host.Tail {
		t.Fatalf("two packages with one salt: %+v vs %+v", other, quota)
	}
	elsewhere := read(two, n2.ID, "acme_quota")
	if *elsewhere.Host != *quota.Host || elsewhere.Cards[0] != quota.Cards[0] {
		t.Fatalf("one package on two servers: %+v vs %+v", elsewhere, quota)
	}
	// The plugin's key is not the bare salt a compiled module would pass.
	bare, err := one.NodeIdentity(ctx, []uint{n1.ID}, []byte("shared-salt"))
	if err != nil || bare[0].Host.H == quota.Host.H {
		t.Fatalf("bare salt = %+v, %v", bare, err)
	}
	// A node the agent has not reported a machine for has no host part.
	n3, _, err := one.CreateNodePending("gpu-a02")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(one, n3.ID, "acme_quota"); got.Host != nil || got.Cards == nil {
		t.Fatalf("unreported node = %+v", got)
	}
}

// The inventory page carries what the store pages, with no raw serial, a
// cursor for the next page and a valid wire shape.
func TestPluginReadsPageTheInventory(t *testing.T) {
	ctx := context.Background()
	st, n1 := readsStore(t, "inv")
	n2, _, err := st.CreateNodePending("gpu-a02")
	if err != nil {
		t.Fatal(err)
	}
	reads := PluginReads(st)
	nodes, next, err := reads.NodeInventory(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	page := extapi.NodesPage{Nodes: nodes}
	if err := page.Validate(); err != nil || len(nodes) != 1 || next != uint64(n1.ID) {
		t.Fatalf("first page %+v next %d: %v", nodes, next, err)
	}
	got := nodes[0]
	if got.ID != uint64(n1.ID) || got.UUID != n1.UUID || got.Name != "gpu-a01" || !got.HostReported || got.EnrolledAt.IsZero() ||
		len(got.Cards) != 2 || got.Cards[0] != (extapi.InventoryCard{Index: 0, Model: "H800", Serial: true}) ||
		got.Cards[1] != (extapi.InventoryCard{Index: 1, Model: "910B", Serial: false}) {
		t.Fatalf("node = %+v", got)
	}
	raw, _ := extapi.Marshal(page)
	if strings.Contains(string(raw), "8f3c1a2b") || strings.Contains(strings.ToLower(string(raw)), "4c4c4544") {
		t.Fatalf("a raw serial leaked: %s", raw)
	}
	nodes, next, err = reads.NodeInventory(ctx, next, 500)
	if err != nil || len(nodes) != 1 || nodes[0].ID != uint64(n2.ID) || next != 0 || nodes[0].HostReported || nodes[0].Cards == nil {
		t.Fatalf("last page %+v next %d: %v", nodes, next, err)
	}
	if _, _, err := reads.NodeInventory(ctx, 0, 501); err == nil {
		t.Fatal("an over-long page was not refused")
	}
}

// Text longer than the wire allows is cut at a rune boundary.
func TestClipTextKeepsUTF8(t *testing.T) {
	s := strings.Repeat("节", 100) // 300 bytes
	got := clipText(s, extapi.MaxNodeText)
	if len(got) > extapi.MaxNodeText || !utf8.ValidString(got) || len(got) != 255 {
		t.Fatalf("clipped to %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if clipText("gpu", 255) != "gpu" {
		t.Fatal("short text changed")
	}
}
