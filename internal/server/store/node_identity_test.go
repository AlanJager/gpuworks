package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// reportIdentityCards writes one status report of cards, the way an agent
// would.
func reportIdentityCards(t *testing.T, s *Store, uuid string, cards ...proto.GPUInfo) {
	t.Helper()
	ok := true
	if err := s.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
}

func identityCard(index int, uuid, bdf, vendor, ownership string) proto.GPUInfo {
	return proto.GPUInfo{Index: index, UUID: uuid, PCIeBDF: bdf, Vendor: vendor, Name: "NVIDIA H800", MemoryTotalBytes: 80 << 30, Healthy: true, Ownership: ownership}
}

func wantPart(salt, kind, raw string) IdentityPart {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(kind + "\x00" + raw))
	return IdentityPart{H: hex.EncodeToString(mac.Sum(nil))[:16], Tail: raw[len(raw)-4:]}
}

// identityFleet is three nodes: two logical nodes on one machine, each
// owning half its cards, and an Ascend node whose card ids are indexes and
// whose agent reported no host UUID.
func identityFleet(t *testing.T, s *Store) (left, right, ascend *Node) {
	t.Helper()
	var err error
	if left, _, err = s.CreateNodePending("left"); err != nil {
		t.Fatal(err)
	}
	if right, _, err = s.CreateNodePending("right"); err != nil {
		t.Fatal(err)
	}
	if ascend, _, err = s.CreateNodePending("ascend"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*Node{left, right} {
		if _, err := s.SetNodeHostSystemUUID(n.UUID, "4C4C4544-0042-3510-8051-B4C04F4E3432"); err != nil {
			t.Fatal(err)
		}
	}
	a := "GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8"
	b := "GPU-0a1b2c3d-0000-1111-2222-333344445555"
	reportIdentityCards(t, s, left.UUID,
		identityCard(0, a, "0000:3b:00.0", "nvidia", ""),
		identityCard(1, b, "0000:3c:00.0", "nvidia", accelerator.CardHeldElsewhere))
	reportIdentityCards(t, s, right.UUID,
		identityCard(0, a, "0000:3b:00.0", "nvidia", accelerator.CardHeldElsewhere),
		identityCard(1, b, "0000:3c:00.0", "nvidia", ""))
	reportIdentityCards(t, s, ascend.UUID,
		identityCard(0, "ascend-0", "0000:c1:00.0", "ascend", ""),
		identityCard(1, "ascend-1", "0000:c2:00.0", "ascend", ""))
	return left, right, ascend
}

// Each node's identity is its machine's UUID and the serials of the cards
// it owns, salted with the caller's salt; raw values appear only as their
// last four characters. Two logical nodes on one machine share the host
// digest and split the cards; slot-derived ids are counted, not digested.
func TestNodeIdentityDigestsOwnedHardwareWithTheCallersSalt(t *testing.T) {
	s := newTestStore(t)
	left, right, ascend := identityFleet(t, s)
	got, err := s.NodeIdentity(context.Background(), []uint{ascend.ID, right.ID, left.ID, left.ID}, []byte("salt-one"))
	if err != nil {
		t.Fatal(err)
	}
	host := wantPart("salt-one", "host", "4c4c4544-0042-3510-8051-b4c04f4e3432")
	if len(got) != 3 || got[0].NodeID != left.ID || got[1].NodeID != right.ID || got[2].NodeID != ascend.ID {
		t.Fatalf("identities = %+v, want one per node in id order", got)
	}
	if got[0].Host != host || got[1].Host != host || host.Tail != "3432" {
		t.Fatalf("host parts = %+v / %+v, want %+v", got[0].Host, got[1].Host, host)
	}
	if want := wantPart("salt-one", "card", "GPU-8f3c1a2b-4d5e-6f70-8192-a3b4c5d6e7f8"); len(got[0].Cards) != 1 || got[0].Cards[0] != want || got[0].CardsWithoutSerial != 0 {
		t.Fatalf("left cards = %+v (+%d), want [%+v]", got[0].Cards, got[0].CardsWithoutSerial, want)
	}
	if want := wantPart("salt-one", "card", "GPU-0a1b2c3d-0000-1111-2222-333344445555"); len(got[1].Cards) != 1 || got[1].Cards[0] != want {
		t.Fatalf("right cards = %+v, want [%+v]", got[1].Cards, want)
	}
	if got[2].Host.Known() || got[2].Host != (IdentityPart{}) || len(got[2].Cards) != 0 || got[2].Cards == nil || got[2].CardsWithoutSerial != 2 {
		t.Fatalf("ascend identity = %+v, want an unknown host, no digests and two cards without a serial", got[2])
	}
	for _, id := range got {
		for _, p := range append([]IdentityPart{id.Host}, id.Cards...) {
			if strings.Contains(p.H, "4c4c4544") || len(p.Tail) > 4 {
				t.Fatalf("a raw value leaked: %+v", p)
			}
		}
	}

	other, err := s.NodeIdentity(context.Background(), []uint{left.ID}, []byte("salt-two"))
	if err != nil {
		t.Fatal(err)
	}
	if other[0].Host.H == host.H || other[0].Host.Tail != host.Tail || other[0].Cards[0].H == got[0].Cards[0].H {
		t.Fatalf("another salt must give unrelated digests with the same tails: %+v vs %+v", other[0], got[0])
	}
	again, _ := s.NodeIdentity(context.Background(), []uint{left.ID}, []byte("salt-one"))
	if again[0].Host != got[0].Host || again[0].Cards[0] != got[0].Cards[0] {
		t.Fatal("the same salt must give the same digests")
	}
}

// A partial answer: a node that does not exist is left out, and the rest
// still answer; a call that names none of them answers empty, not nil.
func TestNodeIdentityLeavesOutNodesThatDoNotExist(t *testing.T) {
	s := newTestStore(t)
	left, right, _ := identityFleet(t, s)
	if err := s.DeleteNode(right.UUID); err != nil {
		t.Fatal(err)
	}
	got, err := s.NodeIdentity(context.Background(), []uint{9999, right.ID, left.ID}, []byte("salt"))
	if err != nil || len(got) != 1 || got[0].NodeID != left.ID {
		t.Fatalf("identities = %+v, %v; want only the left node", got, err)
	}
	none, err := s.NodeIdentity(context.Background(), []uint{9999}, []byte("salt"))
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("unknown nodes = %#v, %v; want an empty slice", none, err)
	}
}

// The batch and salt bounds are refused, never truncated, and a cancelled
// context fails the read.
func TestNodeIdentityRefusesOutOfBoundsCalls(t *testing.T) {
	s := newTestStore(t)
	left, _, _ := identityFleet(t, s)
	many := make([]uint, MaxNodeReadBatch+1)
	for i := range many {
		many[i] = uint(i + 1)
	}
	for name, tc := range map[string]struct {
		ids  []uint
		salt []byte
		want error
	}{
		"no ids":        {nil, []byte("salt"), ErrNodeReadBatch},
		"only id zero":  {[]uint{0}, []byte("salt"), ErrNodeReadBatch},
		"too many ids":  {many, []byte("salt"), ErrNodeReadBatch},
		"empty salt":    {[]uint{left.ID}, nil, ErrIdentitySalt},
		"oversize salt": {[]uint{left.ID}, make([]byte, MaxIdentitySalt+1), ErrIdentitySalt},
	} {
		if got, err := s.NodeIdentity(context.Background(), tc.ids, tc.salt); !errors.Is(err, tc.want) || got != nil {
			t.Errorf("%s: = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if _, err := s.NodeIdentity(context.Background(), many[:MaxNodeReadBatch], make([]byte, MaxIdentitySalt)); err != nil {
		t.Fatalf("the largest allowed call: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.NodeIdentity(ctx, []uint{left.ID}, []byte("salt")); err == nil {
		t.Fatal("a cancelled context must fail the read")
	}
}

// Both identity reads are pinned: primary-key lookups and the gpus
// (node_id, ident) index, no scan of either table and no sort b-tree.
func TestNodeIdentityAndInventoryQueryPlans(t *testing.T) {
	s := newTestStore(t)
	for name, query := range map[string]string{
		"hosts":     strings.Replace(nodeIdentityHostsSQL, "?", "(1, 2, 3)", 1),
		"cards":     strings.Replace(strings.Replace(ownedCardsSQL, "?", "(1, 2, 3)", 1), "?", "''", 1),
		"inventory": strings.Replace(strings.Replace(nodeInventorySQL, "?", "0", 1), "?", "501", 1),
	} {
		plan := explainPlan(t, s, query)
		t.Logf("%s: %s", name, plan)
		if strings.Contains(plan, "SCAN") || strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "SEARCH") {
			t.Errorf("%s plan = %s", name, plan)
		}
	}
}
