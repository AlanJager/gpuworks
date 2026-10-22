package app

import (
	"context"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Whether each of this node's cards can actually run work, asked by trying
// it, and folded into what the node reports about that card.
//
// This lives in the assembly layer because it is where the pieces meet: the
// prober says which cards exist, the engine says which are serving, and the
// vendor's own runtime check says whether a card answers. None of those
// three knows about the others.

// startCardSmoke runs the periodic per-card runtime check, on the vendors
// whose runtime can be asked on the host at all
// (accelerator.VendorHasHostRuntimeCheck). A card on any other vendor keeps
// the verdict its CLI gave it.
func (n *NodeRuntime) startCardSmoke(ctx context.Context) {
	if n.prober == nil || !accelerator.VendorHasHostRuntimeCheck(n.prober.Vendor()) {
		return
	}
	// Hermetic probers describe cards that do not exist; asking the host's
	// libcuda about them would test the host, not the fixture.
	if gpu.IsSynthetic(n.prober) {
		return
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: n.smokeCards,
		Try:   func(ctx context.Context, card cardsmoke.Card) error { return gpu.CUDARuntimeSmokeOn(ctx, card.Visible) },
		Busy: func(card cardsmoke.Card) bool {
			// A card whose owner is not known yet is left alone: creating
			// a context on a card that is serving can disturb it.
			ours, known := n.cardsInUse()
			return !known || ours[card.Ident]
		},
		Unavailable: n.smokeUnavailable,
	})
	go n.cardSmoke.Run(ctx)
}

// smokeCards are the cards worth asking: this node's own, named by UUID so
// the runtime cannot be pointed at a different card than the one meant —
// a device index shifts whenever the set of working cards changes.
func (n *NodeRuntime) smokeCards(ctx context.Context) []cardsmoke.Card {
	gpus, err := n.prober.Probe(ctx)
	if err != nil && len(gpus) == 0 {
		return nil
	}
	cards := make([]cardsmoke.Card, 0, len(gpus))
	for _, g := range gpus {
		if !accelerator.CardUsable(g.Ownership) || g.UUID == "" {
			// A card this node does not own cannot be tried, and one with
			// no UUID cannot be named to the runtime without risking
			// another card.
			continue
		}
		if ident := accelerator.CardIdent(g.UUID, g.PCIeBDF); ident != "" {
			cards = append(cards, cardsmoke.Card{Ident: ident, Visible: g.UUID})
		}
	}
	return cards
}

// smokeUnavailable says why no card can be tried right now, or "". A driver
// that is not answering would make every attempt queue on the same lock,
// and a host without python3 or libcuda cannot run the check at all.
func (n *NodeRuntime) smokeUnavailable() string {
	if gpu.VendorCLISlow("nvidia-smi") {
		return "the accelerator driver is not answering; the runtime check waits for it"
	}
	if reason := gpu.CUDASmokePrerequisite(); reason != "" {
		return reason
	}
	return ""
}

// CardHealthFindings folds this agent's own verdicts into the cards it is
// about to report. The probe states what the vendor tool says about a card;
// this adds what trying the card said, and the more severe of the two is
// what the card carries (accelerator.MostSevereHealth), because a card the
// tool calls healthy and the runtime cannot use is not healthy.
func (n *NodeRuntime) CardHealthFindings(ctx context.Context, gpus []proto.GPUInfo, probeOK bool) {
	n.applyForeignProcesses(ctx, gpus, probeOK)
	if n.cardSmoke == nil {
		return
	}
	outcomes := n.cardSmoke.Outcomes()
	if len(outcomes) == 0 {
		return
	}
	for i := range gpus {
		card := &gpus[i]
		if !accelerator.CardUsable(card.Ownership) {
			// Tried while it was this node's and since lent to a virtual
			// machine or let go: what it answered then is not this node's
			// to state about it now.
			continue
		}
		outcome, ok := outcomes[accelerator.CardIdent(card.UUID, card.PCIeBDF)]
		if !ok || outcome.OK {
			// Never tried, or tried and fine: the card keeps the verdict
			// the vendor tool gave it. A check that did not run must not
			// read as a card that failed.
			continue
		}
		// The order of findings says which one an operator needs to read
		// (accelerator.healthSeverity): this one outranks anything that
		// would leave the card looking schedulable, and yields to a vendor
		// finding that names why the card is unusable rather than that it
		// is.
		if accelerator.MostSevereHealth(card.HealthCode, accelerator.HealthRuntimeUnusable) != accelerator.HealthRuntimeUnusable {
			continue
		}
		gpu.SetCardFinding(card, accelerator.HealthRuntimeUnusable, []accelerator.HealthField{
			{Name: "Runtime Check", Value: "failed"},
			{Name: "Checked At", Value: outcome.At.UTC().Format("2006-01-02 15:04:05Z")},
			{Name: "Detail", Value: strings.TrimSpace(outcome.Detail)},
		})
	}
}
