package app

import (
	"context"
	"log/slog"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Who else is using this node's cards.
//
// A node is not always the platform's alone: somebody logs in and starts a
// training script or a vLLM of their own. That card's memory is then spoken
// for by something the platform cannot see, let alone plan around, so
// placing a replica on it ends in an out-of-memory failure at startup — the
// platform's own accounting said the card was free.
//
// A process is "foreign" when the card it runs on holds none of this
// agent's instances. That is deliberately coarser than matching process
// IDs to containers: it never mistakes an idle card for a busy one, which
// is the error that would cost a deployment, and it needs no cgroup
// parsing. Its cost is that a card already running one of our instances is
// not examined for a second, foreign workload — and such a card is already
// accounted for and not offered again.

// cardOccupancy is the last occupancy this agent established, by card
// identity. The server writes a report's occupancy over what it had, so a
// round that could not find out — the vendor tool failed or timed out, or
// the agent has just started and does not know its own instances yet —
// must restate the last answer rather than send none: none reads as "free",
// and a card somebody else is using would be offered for a deployment.
type cardOccupancy struct {
	mu   sync.Mutex
	last map[string][]proto.CardProcess
}

// remember records a round that did find out, and reports which cards'
// foreign use changed since the last one, so it is logged once rather than
// every minute.
func (o *cardOccupancy) remember(gpus []proto.GPUInfo) (changed []proto.GPUInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := map[string][]proto.CardProcess{}
	for _, card := range gpus {
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || len(card.ForeignProcesses) == 0 {
			continue
		}
		next[ident] = card.ForeignProcesses
		if !sameProcesses(o.last[ident], card.ForeignProcesses) {
			changed = append(changed, card)
		}
	}
	o.last = next
	return changed
}

// restate lays the last answer over cards this round could not examine.
func (o *cardOccupancy) restate(gpus []proto.GPUInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range gpus {
		card := &gpus[i]
		if procs, ok := o.last[accelerator.CardIdent(card.UUID, card.PCIeBDF)]; ok && accelerator.CardUsable(card.Ownership) {
			card.ForeignProcesses = append([]proto.CardProcess(nil), procs...)
		}
	}
}

func sameProcesses(a, b []proto.CardProcess) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PID != b[i].PID || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// applyForeignProcesses marks the cards somebody outside the platform is
// using. It mutates the cards in place, like the other findings this agent
// contributes to its own report.
func (n *NodeRuntime) applyForeignProcesses(ctx context.Context, gpus []proto.GPUInfo, probeOK bool) {
	lister, ok := n.prober.(gpu.ComputeProcessLister)
	if !ok || len(gpus) == 0 {
		return
	}
	if !probeOK {
		// The tool just failed to list the cards; asking it which
		// processes hold them spends the whole vendor timeout to be told
		// the same thing.
		slog.Debug("card occupancy restated: the accelerator probe failed this round")
		n.occupancy.restate(gpus)
		return
	}
	procs, err := lister.ComputeProcesses(ctx)
	if err != nil {
		// Not knowing is not the same as knowing a card is free.
		slog.Debug("compute processes unavailable; card occupancy restated", "err", err)
		n.occupancy.restate(gpus)
		return
	}
	ours, known := n.cardsInUse()
	if !known {
		// This agent has just started and has not been told what it should
		// be running. Every card serving one of our own instances would
		// read as somebody else's, and the cards the platform is using
		// most would be the ones it stopped scheduling.
		slog.Debug("card occupancy restated: this agent's own instances are not known yet")
		n.occupancy.restate(gpus)
		return
	}
	for i := range gpus {
		card := &gpus[i]
		if !accelerator.CardUsable(card.Ownership) || ours[accelerator.CardIdent(card.UUID, card.PCIeBDF)] {
			continue
		}
		for _, proc := range procs {
			if !gpu.SameBDF(proc.PCIeBDF, card.PCIeBDF) {
				continue
			}
			card.ForeignProcesses = append(card.ForeignProcesses, proto.CardProcess{
				PID: proc.PID, Name: proc.Name, MemoryBytes: proc.MemoryBytes,
			})
		}
	}
	for _, card := range n.occupancy.remember(gpus) {
		slog.Info("card is in use outside the platform",
			"card", card.PCIeBDF, "processes", len(card.ForeignProcesses))
	}
}

// cardsInUse are the hardware identities this agent's own instances
// occupy, and whether that is known at all. It is the one answer to "is
// this card ours already", asked both by the runtime check (which leaves
// such a card alone) and by the occupancy report (which does not call its
// processes foreign).
//
// Identities, not device indexes: an index is placement-time and drifts,
// and comparing a drifted one called the wrong cards ours.
//
// known is false until the control plane has told this agent what to run.
// An agent that has just restarted still has its instances running on the
// cards, so an empty set read as "none of these are ours" would call every
// one of them foreign — measured on gpu3, where both cards were reported
// as used outside the platform for one report after an agent upgrade.
//
// heldCardIdents is a seam: the engine starts containers, which a test of
// these rules has no business doing.
func (n *NodeRuntime) cardsInUse() (cards map[string]bool, known bool) {
	ours := map[string]bool{}
	add := func(idents []string) {
		for _, ident := range idents {
			if ident != "" {
				ours[ident] = true
			}
		}
	}
	if held := n.heldCardIdents; held != nil {
		add(held())
		return ours, true
	}
	if n.engine == nil {
		return ours, false
	}
	add(n.engine.HeldCardIdents())
	return ours, n.engine.DesiredKnown()
}
