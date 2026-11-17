package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func TestPushedStateHooksAdaptWithoutAllocating(t *testing.T) {
	st, err := store.Open("file:pushed_state_hooks?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	str := func(s string) *string { return &s }
	maxNodes := int64(1)

	// What a plugin pushed before the restart: a hold, a cap and a banner.
	holds := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_quota", Kind: extapi.KindNodeHolds, Schema: 1, Generation: 1, Digest: "sha256:x"}}
	holds.Insert.NodeHolds = []pstate.NodeHold{{Owner: "acme_quota", NodeID: 9, Code: "ACME_MAINTENANCE"}}
	holds.Insert.Params = []pstate.NoticeParam{{Owner: "acme_quota", Kind: extapi.KindNodeHolds, Entry: "9", Slot: pstate.SlotNode, Key: "ticket", Str: str("OPS-1")}}
	capacity := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_quota", Kind: extapi.KindFleetCapacity, Schema: 1, Generation: 1, Digest: "sha256:y"}}
	capacity.Insert.FleetCapacity = []pstate.FleetCapacity{{Owner: "acme_quota", Select: extapi.SelectEnrollmentOrder, RefuseNodeCode: "ACME_NODE_LIMIT", RefuseCardCode: "ACME_CARD_LIMIT", HoldCode: "ACME_OVER_CAPACITY"}}
	capacity.Insert.CapacitySteps = []pstate.CapacityStep{{Owner: "acme_quota", MaxNodesWithCards: &maxNodes}}
	banners := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_quota", Kind: extapi.KindBanners, Schema: 1, Generation: 1, Digest: "sha256:z"}}
	banners.Insert.Banners = []pstate.Banner{{Owner: "acme_quota", BannerID: "grace", Audience: extapi.AudienceAdmin}}
	banners.Insert.BannerSteps = []pstate.BannerStep{{Owner: "acme_quota", BannerID: "grace", Severity: extapi.SeverityWarning, NoticeCode: "ACME_GRACE"}}
	if _, err := st.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{holds, capacity, banners}}); err != nil {
		t.Fatal(err)
	}

	grants := extapi.CertGrants{IDPrefixes: []string{"acme_"}, FeatureNS: []string{"acme"}, AuthorityID: "acme_quota", Tables: []string{extapi.KindFleetCapacity}, MinIsolation: extapi.IsolationLandlock}
	state := pushstate.New(pushstate.Config{
		Store: st, Fleet: PushedStateFleet(st), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Packages: []pushstate.Package{{ID: "acme_quota", Publisher: "acme", Grants: grants, Verified: true,
			Features: []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority}}}},
	})
	if err := state.Load(ctx); err != nil {
		t.Fatal(err)
	}
	hooks := NewPushedStateHooks(state)
	holder := hooks.Holder()
	if holder.Source() != pushstate.HoldSource {
		t.Fatalf("source = %q", holder.Source())
	}
	if err := st.AddSchedulingHolder(holder); err != nil {
		t.Fatalf("the store refuses the holder: %v", err)
	}
	st.AddNodeEnrollmentGuard(hooks.GuardNode)

	hold := holder.Hold(store.NodeHoldFacts{ID: 9, OwnedCards: 8})
	if !hold.Held() || hold.ProductError().Code() != "ACME_MAINTENANCE" || hold.Params["ticket"] != "OPS-1" {
		t.Fatalf("hold = %+v", hold)
	}
	if refusal := hooks.GuardNode(store.FleetCapacity{NodesWithCards: 1}); refusal == nil || refusal.Code() != "ACME_NODE_LIMIT" {
		t.Fatalf("guard = %v", refusal)
	}
	if a := hooks.Features().Availability("acme.backup"); a.Access != featuregate.Locked {
		t.Fatalf("availability = %+v", a)
	}
	admin, member := hooks.Banners().Banners(ctx, auth.Claims{Role: adminRole}), hooks.Banners().Banners(ctx, auth.Claims{Role: "user"})
	if len(admin) != 1 || admin[0].ID != "acme_quota.grace" || admin[0].Message.Code != "ACME_GRACE" || len(member) != 0 {
		t.Fatalf("banners: admin=%+v member=%+v", admin, member)
	}

	var (
		held    store.NodeHold
		refusal any
		av      featuregate.Availability
	)
	for name, fn := range map[string]func(){
		"hold":    func() { held = holder.Hold(store.NodeHoldFacts{ID: 9, OwnedCards: 8}) },
		"no hold": func() { held = holder.Hold(store.NodeHoldFacts{ID: 3, OwnedCards: 0}) },
		"guard":   func() { refusal = hooks.GuardNode(store.FleetCapacity{}) },
		"feature": func() { av = hooks.Features().Availability("acme.backup") },
	} {
		if n := testing.AllocsPerRun(1000, fn); n != 0 {
			t.Errorf("%s: %v allocations per call", name, n)
		}
	}
	_, _, _ = held, refusal, av

	// The fleet reader converts the store's fleet in enrollment order.
	for _, uuid := range []string{"n-1", "n-2"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := PushedStateFleet(st)(ctx)
	if err != nil || len(fleet) != 2 || fleet[0].ID >= fleet[1].ID {
		t.Fatalf("fleet = %+v, %v", fleet, err)
	}
}
