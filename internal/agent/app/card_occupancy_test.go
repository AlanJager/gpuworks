package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// occupiedProber reports the compute processes it is given.
type occupiedProber struct {
	gpu.FixedProber
	procs []gpu.ComputeProcess
	err   error
}

func (p *occupiedProber) ComputeProcesses(context.Context) ([]gpu.ComputeProcess, error) {
	return p.procs, p.err
}

// occupancyNode is an agent that has been told what it should be running
// — nothing, unless a case says otherwise. A node that has not been told
// yet is its own case (TestAnAgentThatDoesNotYetKnowItsOwnInstances...).
func occupancyNode(t *testing.T, cards []proto.GPUInfo, procs []gpu.ComputeProcess, err error) *NodeRuntime {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: cards}, procs: procs, err: err}, nil)
	n.heldCardIdents = func() []string { return nil }
	return n
}

func twoOwnedCards() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
		{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", Healthy: true},
	}
}

// Somebody logged into the node and started their own workload. Its memory
// is spoken for by something the platform cannot plan around, so the card
// is reported as taken, with who has it.
func TestACardUsedOutsideThePlatformIsReportedWithWhoHasIt(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		// The vendor tool prints eight domain digits; the card's own
		// address has four.
		{PID: 4242, Name: "python3", PCIeBDF: "00000000:40:00.0", MemoryBytes: 20 << 30},
	}, nil)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)

	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("the idle card = %+v, want nothing on it", cards[0].ForeignProcesses)
	}
	taken := cards[1].ForeignProcesses
	if len(taken) != 1 || taken[0].PID != 4242 || taken[0].Name != "python3" || taken[0].MemoryBytes != 20<<30 {
		t.Fatalf("the taken card = %+v", taken)
	}
}

// Not knowing is not the same as knowing a card is free: a vendor tool that
// cannot be asked leaves every card as the last report left it.
func TestAnUnavailableProcessListLeavesCardsAlone(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), nil, errors.New("nvidia-smi: no answer within 10s"))
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card = %+v, want nothing claimed when the tool could not be asked", card)
		}
	}
}

// An agent that has just restarted has its instances still running on the
// cards while the control plane has not yet told it what they are. Reading
// "I am told of no instances" as "none of these cards are mine" calls every
// card the platform is using somebody else's, and stops scheduling on
// exactly the busiest node — seen on gpu3 after an agent upgrade.
func TestAnAgentThatDoesNotYetKnowItsOwnInstancesClaimsNothing(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
			procs: []gpu.ComputeProcess{{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0"}}}, nil)
	if ours, known := n.cardsInUse(); known || len(ours) != 0 {
		t.Fatalf("cardsInUse = %v known=%v, want it to admit it does not know", ours, known)
	}
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card %s = %+v, want nothing claimed before this agent knows its own instances",
				card.PCIeBDF, card.ForeignProcesses)
		}
	}
}

// A card this node does not own is not examined: it is somebody else's
// hardware and its processes are not ours to report.
func TestCardsOwnedElsewhereAreNotExamined(t *testing.T) {
	cards := []proto.GPUInfo{
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
	}
	n := occupancyNode(t, cards, []gpu.ComputeProcess{{PID: 1, Name: "qemu", PCIeBDF: "0000:1b:00.0"}}, nil)
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("a passed-through card = %+v, want its VM's processes not reported as foreign", cards[0].ForeignProcesses)
	}
}

// A prober whose tool does not list processes at all reports nothing, and
// nothing breaks.
func TestAProberWithoutProcessListingReportsNothing(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "ascend", GPUs: twoOwnedCards()}, nil)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card = %+v", card)
		}
	}
}

// A card already running one of this platform's instances is not examined:
// its processes are ours, and the card is accounted for and not offered
// again anyway.
func TestACardRunningOurOwnInstanceHasNoForeignProcesses(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0", MemoryBytes: 20 << 30},
		{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0", MemoryBytes: 8 << 30},
	}, nil)
	// The card at 3f runs one of ours, named by its identity rather than
	// by a device index that drifts.
	n.heldCardIdents = func() []string { return []string{accelerator.CardIdent("", "0000:3f:00.0")} }

	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("our own instance was reported as foreign: %+v", cards[0].ForeignProcesses)
	}
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("the card nothing of ours runs on = %+v, want the foreign process named", cards[1].ForeignProcesses)
	}
}

// The server writes each report's occupancy over what it had, so a round
// that could not find out must say what the last one found. Saying nothing
// read as "free", and the card somebody else was using went to a
// deployment one report after the vendor tool first timed out.
func TestARoundThatCannotFindOutRestatesTheLastAnswer(t *testing.T) {
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0"}}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", prober, nil)
	n.heldCardIdents = func() []string { return nil }
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("first round = %+v", cards[1].ForeignProcesses)
	}

	for _, round := range []struct {
		name    string
		probeOK bool
		err     error
	}{
		{"the process list timed out", true, errors.New("nvidia-smi: no answer within 10s")},
		{"the card probe itself failed", false, nil},
	} {
		prober.err = round.err
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, round.probeOK)
		if got := cards[1].ForeignProcesses; len(got) != 1 || got[0].PID != 4242 {
			t.Fatalf("%s: the taken card = %+v, want the last answer restated", round.name, got)
		}
		if len(cards[0].ForeignProcesses) != 0 {
			t.Fatalf("%s: the idle card = %+v", round.name, cards[0].ForeignProcesses)
		}
	}

	// A round that does find out replaces the answer, including with
	// "nobody": the process went away and the card is free again.
	prober.err, prober.procs = nil, nil
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 0 {
		t.Fatalf("after the process left = %+v, want the card free", cards[1].ForeignProcesses)
	}
	prober.err = errors.New("timeout")
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 0 {
		t.Fatalf("a later failed round brought back %+v", cards[1].ForeignProcesses)
	}
}
