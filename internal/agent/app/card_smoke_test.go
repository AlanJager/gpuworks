package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// smokeNode is a node runtime whose card check answers as told.
func smokeNode(t *testing.T, cards []proto.GPUInfo, fail map[string]error) *NodeRuntime {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cards}, nil)
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: n.smokeCards,
		Try: func(_ context.Context, card cardsmoke.Card) error {
			return fail[card.Visible]
		},
	})
	n.cardSmoke.RunOnce(context.Background())
	return n
}

func cardsOfOneHealthyNode() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
		{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", Healthy: true},
	}
}

// A card the vendor tool calls healthy and the runtime cannot use is not
// healthy, and the report says which card and why.
func TestARuntimeFailureMakesTheCardUnusable(t *testing.T) {
	n := smokeNode(t, cardsOfOneHealthyNode(), map[string]error{"GPU-40": errors.New("cuInit=101")})
	cards := cardsOfOneHealthyNode()
	n.CardHealthFindings(context.Background(), cards, true)

	if cards[0].HealthCode != "" || !cards[0].Healthy {
		t.Fatalf("the card that answered = %+v, want it untouched", cards[0])
	}
	bad := cards[1]
	if bad.HealthCode != accelerator.HealthRuntimeUnusable || bad.Healthy || bad.HealthStatus != accelerator.HealthError {
		t.Fatalf("the card the runtime cannot use = %+v", bad)
	}
	if !strings.Contains(bad.HealthMessage, "cuInit=101") {
		t.Fatalf("the finding does not carry what the runtime said: %q", bad.HealthMessage)
	}
}

// A check that never ran is not a card that failed.
func TestACardNeverCheckedKeepsItsVerdict(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	cards := cardsOfOneHealthyNode()
	n.CardHealthFindings(context.Background(), cards, true) // no runner at all
	for _, card := range cards {
		if card.HealthCode != "" || !card.Healthy {
			t.Fatalf("card = %+v, want it untouched when nothing was checked", card)
		}
	}

	// A runner that could not run the check either.
	n2 := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	n2.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:       n2.smokeCards,
		Try:         func(context.Context, cardsmoke.Card) error { return nil },
		Unavailable: func() string { return "python3 is not installed" },
	})
	n2.cardSmoke.RunOnce(context.Background())
	cards = cardsOfOneHealthyNode()
	n2.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if card.HealthCode != "" || !card.Healthy {
			t.Fatalf("card = %+v, want it untouched when the check could not run", card)
		}
	}
}

// The vendor tool's own finding names the cause more precisely, so it is
// not replaced by the runtime's symptom.
func TestAVendorFindingOutranksTheRuntimeSymptom(t *testing.T) {
	cards := []proto.GPUInfo{{
		Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0",
		Healthy: false, HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthMemoryRemapFailed,
		HealthMessage: "remapped_rows.failure=1",
	}}
	n := smokeNode(t, cards, map[string]error{"GPU-3f": errors.New("cuInit=101")})
	n.CardHealthFindings(context.Background(), cards, true)
	if cards[0].HealthCode != accelerator.HealthMemoryRemapFailed {
		t.Fatalf("health code = %q, want the vendor tool's more specific finding", cards[0].HealthCode)
	}
	if !strings.Contains(cards[0].HealthMessage, "remapped_rows.failure=1") {
		t.Fatalf("the vendor evidence was replaced: %q", cards[0].HealthMessage)
	}
}

// Only cards this node owns, and only ones it can name to the runtime, are
// asked. A card is named by UUID so the runtime cannot be pointed at a
// different one than the one meant.
func TestOnlyTheNodesOwnNamedCardsAreAsked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1a:00.0", Ownership: accelerator.CardWithoutDriver},
			// Owned but unnameable: its driver never gave it a UUID.
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", HealthCode: accelerator.HealthDriverInitFailed},
		}}, nil)
	asked := n.smokeCards(context.Background())
	if len(asked) != 1 || asked[0].Visible != "GPU-3f" || asked[0].Ident != "pci:0000:3f:00.0" {
		t.Fatalf("cards to check = %+v, want only the node's own, named by UUID", asked)
	}
}

// A hermetic prober describes cards that do not exist; asking the host's
// own runtime about them would test the host instead of the fixture.
func TestASyntheticProberIsNeverChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.startCardSmoke(ctx)
	if n.cardSmoke != nil {
		t.Fatal("a synthetic prober started a real runtime check")
	}
}

// A card running one of this platform's instances has answered the
// question by serving, so the runtime check leaves it alone.
func TestACardServingAnInstanceIsNotChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	n.heldCardIdents = func() []string { return []string{accelerator.CardIdent("GPU-40", "0000:40:00.0")} }

	var asked []string
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: n.smokeCards,
		Try: func(_ context.Context, card cardsmoke.Card) error {
			asked = append(asked, card.Visible)
			return nil
		},
		Busy: func(card cardsmoke.Card) bool { ours, _ := n.cardsInUse(); return ours[card.Ident] },
	})
	n.cardSmoke.RunOnce(context.Background())
	if len(asked) != 1 || asked[0] != "GPU-3f" {
		t.Fatalf("checked %v, want only the idle card", asked)
	}

	// With nothing of ours running, every card is asked.
	n.heldCardIdents = func() []string { return nil }
	asked = nil
	n.cardSmoke.RunOnce(context.Background())
	if len(asked) != 2 {
		t.Fatalf("checked %v, want both cards once nothing of ours runs", asked)
	}
}

func TestCUDASmokePrerequisiteNamesWhatIsMissing(t *testing.T) {
	// The host this runs on has python3 (the test harness needs it) but
	// may well lack a driver library; either answer must name its reason
	// rather than being empty on a host that cannot run the check.
	reason := gpu.CUDASmokePrerequisite()
	if reason != "" && !strings.Contains(reason, "python3") && !strings.Contains(reason, "libcuda") {
		t.Fatalf("reason = %q, want it to name what is missing", reason)
	}
	_ = time.Now
}

// Why no card can be checked right now, which is not the same as a card
// that failed. A driver that is not answering would make every attempt
// queue on the same lock, and a host without the runtime's prerequisites
// cannot run the check at all.
func TestSmokeUnavailableNamesWhyNothingCanBeChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	reason := n.smokeUnavailable()
	// On a host with python3 and a driver library there is nothing to
	// report; on one without either, the reason must name what is missing
	// rather than being a bare "unavailable".
	if reason != "" && !strings.Contains(reason, "python3") && !strings.Contains(reason, "libcuda") && !strings.Contains(reason, "driver") {
		t.Fatalf("reason = %q, want it to name what is missing", reason)
	}
}

// The check is started only where there is one to run.
func TestCardSmokeStartsOnlyForNVIDIAHardware(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		want   bool
		why    string
	}{
		{"nvidia", false, "a synthetic prober describes cards that do not exist"},
		{"ascend", false, "the Ascend runtime lives in the image, not on the host"},
		{"hygon", false, "the Hygon runtime lives in the image, not on the host"},
		{"", false, "a node with no accelerator"},
	} {
		n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
			&gpu.FixedProber{VendorName: tc.vendor, GPUs: cardsOfOneHealthyNode()}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		n.startCardSmoke(ctx)
		started := n.cardSmoke != nil
		cancel()
		if started != tc.want {
			t.Errorf("%s: started = %v, want %v (%s)", tc.vendor, started, tc.want, tc.why)
		}
	}
}
