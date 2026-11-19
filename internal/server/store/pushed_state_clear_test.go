package store

import (
	"context"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func ownerHoldsWrite(owner string, gen int64, nodes ...int64) pstate.TableWrite {
	w := pstate.TableWrite{Record: pstate.Owner{Owner: owner, Kind: extapi.KindNodeHolds, Schema: 1, Generation: gen, Digest: "sha256:h"}, DeleteAll: true}
	for _, n := range nodes {
		w.Insert.NodeHolds = append(w.Insert.NodeHolds, pstate.NodeHold{Owner: owner, NodeID: n, Code: "ACME_NODE_HELD"})
		w.Insert.Params = append(w.Insert.Params, pstate.NoticeParam{Owner: owner, Kind: extapi.KindNodeHolds, Entry: strconv.FormatInt(n, 10), Slot: "notice", Key: "node", Int: i64(n)})
	}
	return w
}

func ownerCapacityWrite(owner string, gen int64) pstate.TableWrite {
	return pstate.TableWrite{Record: pstate.Owner{Owner: owner, Kind: extapi.KindFleetCapacity, Schema: 1, Generation: gen, Digest: "sha256:c"}, DeleteAll: true,
		Insert: pstate.Rows{
			FleetCapacity: []pstate.FleetCapacity{{Owner: owner, Select: "first_joined", RefuseNodeCode: "ACME_CAP", RefuseCardCode: "ACME_CAP", HoldCode: "ACME_CAP"}},
			CapacitySteps: []pstate.CapacityStep{{Owner: owner, Step: 0, MaxCards: i64(8)}},
		}}
}

// A clear deletes one owner's table of one kind with its notice params and
// its overrides, in the batch's transaction; every other owner's and kind's
// rows stay, clearing it again deletes nothing, and a push in the same batch
// lands on the cleared table whatever its generation.
func TestClearingAPushedTableTouchesOnlyThatOwnerAndKind(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	override := func(owner, kind, entry string) pstate.Override {
		return pstate.Override{Owner: owner, Kind: kind, EntryKey: entry, Generation: 3, Action: pstate.OverrideDelete, ByUser: 1, AtMS: 1000, Reason: "maintenance"}
	}
	if _, err := s.WritePushedState(ctx, &pstate.Batch{
		Tables: []pstate.TableWrite{ownerHoldsWrite("acme_quota", 3, 7, 8), ownerCapacityWrite("acme_quota", 2), ownerHoldsWrite("acme_ops", 1, 9)},
		Down:   []pstate.Down{{Owner: "acme_quota", SinceMS: 500}},
		Overrides: []pstate.Override{
			override("acme_quota", extapi.KindNodeHolds, "7"),
			override("acme_quota", extapi.KindFleetCapacity, "max_cards"),
			override("acme_ops", extapi.KindNodeHolds, "9"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	clear := &pstate.Batch{Clear: []pstate.OwnerKind{{Owner: "acme_quota", Kind: extapi.KindNodeHolds}}}
	if clear.Empty() {
		t.Fatal("a batch that clears a table reports itself empty")
	}
	for range 2 {
		if _, err := s.WritePushedState(ctx, clear); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := s.LoadPushedState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	for _, o := range snap.Owners {
		owners = append(owners, o.Owner+"/"+o.Kind)
	}
	if len(owners) != 2 || owners[0] != "acme_ops/node_holds" || owners[1] != "acme_quota/fleet_capacity" {
		t.Fatalf("owner records after the clear = %v", owners)
	}
	if len(snap.Rows.NodeHolds) != 1 || snap.Rows.NodeHolds[0].Owner != "acme_ops" {
		t.Fatalf("node holds after the clear = %+v", snap.Rows.NodeHolds)
	}
	if len(snap.Rows.Params) != 1 || snap.Rows.Params[0].Owner != "acme_ops" {
		t.Fatalf("notice params after the clear = %+v", snap.Rows.Params)
	}
	if len(snap.Rows.FleetCapacity) != 1 || len(snap.Rows.CapacitySteps) != 1 {
		t.Fatalf("the clear touched another kind: %+v", snap.Rows)
	}
	var left []string
	for _, o := range snap.Overrides {
		left = append(left, o.Owner+"/"+o.Kind)
	}
	if len(left) != 2 || left[0] != "acme_quota/fleet_capacity" || left[1] != "acme_ops/node_holds" {
		t.Fatalf("overrides after the clear = %v", left)
	}
	if len(snap.Down) != 1 {
		t.Fatalf("the clear dropped the down record: %+v", snap.Down)
	}

	// A push of a lower generation than the cleared one is not stale, and
	// a push beside a clear of the same table lands after it.
	res, err := s.WritePushedState(ctx, &pstate.Batch{
		Clear:  []pstate.OwnerKind{{Owner: "acme_ops", Kind: extapi.KindNodeHolds}},
		Tables: []pstate.TableWrite{ownerHoldsWrite("acme_quota", 1, 8), ownerHoldsWrite("acme_ops", 1, 7)},
	})
	if err != nil || len(res.Stale) != 0 {
		t.Fatalf("pushes after a clear = %+v, %v", res, err)
	}
	if snap, err = s.LoadPushedState(ctx); err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows.NodeHolds) != 2 || snap.Rows.NodeHolds[0].Owner != "acme_ops" || snap.Rows.NodeHolds[0].NodeID != 7 ||
		snap.Rows.NodeHolds[1].Owner != "acme_quota" || snap.Rows.NodeHolds[1].NodeID != 8 {
		t.Fatalf("node holds after pushes = %+v", snap.Rows.NodeHolds)
	}
	if len(snap.Overrides) != 1 || snap.Overrides[0].Kind != extapi.KindFleetCapacity {
		t.Fatalf("the clear of acme_ops left its override: %+v", snap.Overrides)
	}
}
