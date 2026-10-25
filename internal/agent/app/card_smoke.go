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

// startCardSmoke runs the periodic per-card runtime check on vendors with a
// host runtime check. Other vendors keep the verdict their CLI gave them.
func (n *NodeRuntime) startCardSmoke(ctx context.Context) {
	if n.prober == nil {
		return
	}
	check, ok := gpu.HostRuntimeCheckFor(n.prober.Vendor())
	if !ok {
		return
	}
	// Hermetic probers describe cards that do not exist; asking the host's
	// own runtime about them would test the host, not the fixture.
	if gpu.IsSynthetic(n.prober) {
		return
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:   cardSmokeAttempt(check),
		Busy: func(card cardsmoke.Card) bool {
			ours, _ := n.cardsInUse()
			return ours[card.Ident]
		},
		// Until the control plane says what this node runs, no card can be
		// told apart from one that is serving, and creating a context on a
		// card that is serving can disturb it. That is the whole round's
		// answer rather than each card's, so the round is retried once
		// instead of leaving every card on the vendor tool's verdict for
		// an interval.
		Deferred: func() string {
			if _, known := n.cardsInUse(); known {
				return ""
			}
			return "what this node runs is not known yet; the runtime check waits for the control plane"
		},
		Unavailable: func() string { return smokeUnavailable(check) },
		Timeout:     check.Timeout,
	})
	go n.cardSmoke.Run(ctx)
}

// cardSmokeAttempt translates a runtime that never reached the card into the
// scheduler's host-level inconclusive answer.
func cardSmokeAttempt(check gpu.HostRuntimeCheck) func(context.Context, cardsmoke.Card) error {
	return func(ctx context.Context, card cardsmoke.Card) error {
		err := check.Try(ctx, card.Visible)
		if reason := gpu.HostRuntimeUnavailable(err); reason != "" {
			return cardsmoke.Inconclusive(reason)
		}
		return err
	}
}

// smokeCards lists only this node's own cards that the vendor check can name
// without risking another card.
func (n *NodeRuntime) smokeCards(ctx context.Context, check gpu.HostRuntimeCheck) []cardsmoke.Card {
	gpus, err := n.prober.Probe(ctx)
	if err != nil && len(gpus) == 0 {
		return nil
	}
	cards := make([]cardsmoke.Card, 0, len(gpus))
	for _, g := range gpus {
		if !accelerator.CardUsable(g.Ownership) {
			continue
		}
		visible, named := check.Visible(g)
		if !named {
			continue
		}
		if ident := accelerator.CardIdent(g.UUID, g.PCIeBDF); ident != "" {
			cards = append(cards, cardsmoke.Card{Ident: ident, Visible: visible})
		}
	}
	return cards
}

// smokeUnavailable says why no card can be tried right now, or "". A driver
// that is not answering would make every attempt queue on the same lock,
// and a host without the vendor runtime cannot run the check at all.
func smokeUnavailable(check gpu.HostRuntimeCheck) string {
	if gpu.VendorCLISlow(check.CLI) {
		return "the accelerator driver is not answering; the runtime check waits for it"
	}
	return check.Prerequisite()
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
