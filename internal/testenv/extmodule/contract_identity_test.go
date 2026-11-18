package extmodule

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// digest is the documented identity digest (docs/dev/extensions.md): the
// first 16 hex characters of HMAC-SHA256(salt, kind || 0x00 || raw).
func digest(salt, kind, raw string) string {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(kind + "\x00" + raw))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// identityNode enrolls a node, reports its cards and, when host is set,
// records the host UUID its agent's hello would carry.
func identityNode(t *testing.T, e *testenv.Env, name, host string, cards ...proto.GPUInfo) *store.Node {
	t.Helper()
	n, _, err := e.Store().CreateNodePending(name)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		if _, err := e.Store().SetNodeHostSystemUUID(n.UUID, host); err != nil {
			t.Fatal(err)
		}
	}
	ok := true
	if err := e.Store().UpdateNodeStatus(n.UUID, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
	return n
}

func card(index int, uuid, vendor string) proto.GPUInfo {
	return proto.GPUInfo{Index: index, UUID: uuid, Vendor: vendor, Name: "H800", PCIeBDF: "0000:3" + string(rune('a'+index)) + ":00.0", MemoryTotalBytes: 80 << 30, Healthy: true}
}

// A module reads node identity from the Host as salted digests with
// four-character tails, never a raw serial; the answer is partial where
// the core knows only part of it (no host UUID yet, cards whose ids are
// slots, nodes that no longer exist), and calls out of bounds fail rather
// than being cut short.
func TestAModuleReadsSaltedNodeIdentityFromTheHost(t *testing.T) {
	mod := &fakeModule{name: "fake"}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	full := identityNode(t, e, "full", "4C4C4544-0042-3510-8051-B4C04F4E3432",
		card(0, "GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8", "nvidia"), card(1, "GPU-0a1b2c3d-0000-1111-2222-333344445555", "nvidia"))
	partial := identityNode(t, e, "partial", "", card(0, "ascend-0", "ascend"), card(1, "ascend-1", "ascend"))
	gone := identityNode(t, e, "gone", "ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8")
	if err := e.Store().DeleteNode(gone.UUID); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	got, err := mod.host.NodeIdentity(ctx, []uint{gone.ID, partial.ID, full.ID}, []byte("fake-salt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].NodeID != full.ID || got[1].NodeID != partial.ID {
		t.Fatalf("identities = %+v, want full and partial in id order, the deleted node left out", got)
	}
	f := got[0]
	if f.Host != (store.IdentityPart{H: digest("fake-salt", "host", "4c4c4544-0042-3510-8051-b4c04f4e3432"), Tail: "3432"}) {
		t.Fatalf("host part = %+v", f.Host)
	}
	want := map[string]string{
		digest("fake-salt", "card", "GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8"): "e7f8",
		digest("fake-salt", "card", "GPU-0a1b2c3d-0000-1111-2222-333344445555"): "5555",
	}
	if len(f.Cards) != 2 || f.Cards[0].H > f.Cards[1].H || f.CardsWithoutSerial != 0 {
		t.Fatalf("card parts = %+v (+%d), want two ordered by digest", f.Cards, f.CardsWithoutSerial)
	}
	for _, c := range f.Cards {
		if want[c.H] != c.Tail {
			t.Fatalf("card part %+v is not a documented digest of a reported card", c)
		}
	}
	p := got[1]
	if p.Host.Known() || len(p.Cards) != 0 || p.CardsWithoutSerial != 2 {
		t.Fatalf("partial identity = %+v, want an unknown host and two cards without a serial", p)
	}

	ids := make([]uint, store.MaxNodeReadBatch+1)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	if _, err := mod.host.NodeIdentity(ctx, ids, []byte("fake-salt")); !errors.Is(err, store.ErrNodeReadBatch) {
		t.Fatalf("501 ids = %v, want ErrNodeReadBatch", err)
	}
	if _, err := mod.host.NodeIdentity(ctx, []uint{full.ID}, nil); !errors.Is(err, store.ErrIdentitySalt) {
		t.Fatalf("no salt = %v, want ErrIdentitySalt", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := mod.host.NodeIdentity(cancelled, []uint{full.ID}, []byte("fake-salt")); err == nil {
		t.Fatal("a cancelled read must fail")
	}
}

// A module walks the whole inventory page by page in enrollment order with
// names, card models and whether each card and host is identified; a bad
// limit fails.
func TestAModulePagesTheNodeInventoryFromTheHost(t *testing.T) {
	mod := &fakeModule{name: "fake"}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	var enrolled []uint
	for _, name := range []string{"n1", "n2", "n3", "n4", "n5"} {
		enrolled = append(enrolled, identityNode(t, e, name, "4c4c4544-0042-3510-8051-b4c04f4e3432", card(0, "GPU-"+name+"-0000-1111-2222-333344445555", "nvidia")).ID)
	}
	ctx := context.Background()
	var walked []store.NodeInventoryNode
	pages := 0
	for cursor := uint(0); ; pages++ {
		page, err := mod.host.NodeInventory(ctx, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, page.Nodes...)
		if page.Next == 0 {
			break
		}
		cursor = page.Next
	}
	if pages != 2 || len(walked) != len(enrolled) {
		t.Fatalf("walked %d nodes in %d further pages, want %d in 2", len(walked), pages, len(enrolled))
	}
	for i, n := range walked {
		if n.ID != enrolled[i] || n.Name != []string{"n1", "n2", "n3", "n4", "n5"}[i] || !n.HostReported ||
			len(n.Cards) != 1 || n.Cards[0] != (store.NodeInventoryCard{Index: 0, Model: "H800", Serial: true}) {
			t.Fatalf("node %d = %+v", i, n)
		}
	}
	if _, err := mod.host.NodeInventory(ctx, 0, store.MaxNodeReadBatch+1); !errors.Is(err, store.ErrNodeReadBatch) {
		t.Fatalf("limit 501 = %v, want ErrNodeReadBatch", err)
	}
}

// A module names a few nodes by id in one read: display names of the nodes
// that exist, nothing for the rest, an empty answer for no ids, and too
// many ids fail.
func TestAModuleReadsNodeNamesByIDFromTheHost(t *testing.T) {
	mod := &fakeModule{name: "fake"}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	a := identityNode(t, e, "a", "")
	b := identityNode(t, e, "b", "")
	gone := identityNode(t, e, "gone", "")
	if err := e.Store().DeleteNode(gone.UUID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := mod.host.NodeNamesByID(ctx, []uint{b.ID, gone.ID, a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID] != "a" || got[b.ID] != "b" {
		t.Fatalf("names = %v, want a and b, the deleted node absent", got)
	}
	if empty, err := mod.host.NodeNamesByID(ctx, nil); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no ids = %#v, %v; want an empty map", empty, err)
	}
	ids := make([]uint, store.MaxNodeReadBatch+1)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	if _, err := mod.host.NodeNamesByID(ctx, ids); !errors.Is(err, store.ErrNodeReadBatch) {
		t.Fatalf("501 ids = %v, want ErrNodeReadBatch", err)
	}
}
