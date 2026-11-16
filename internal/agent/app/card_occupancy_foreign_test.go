package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The server may place a memory-limited partition beside processes outside
// the platform (ADR 0068), counting what they hold. Once our instance is on
// the card the agent no longer examines it - every process there could be
// ours - so it must keep naming the processes it already knew were somebody
// else's, with what they hold now, or the server would read the card as
// free after the first placement.
//
// The holder is the one of flitai-io/gpuworks#20: a GPUStack container's
// python3 on an H20-3e.
func TestAForeignProcessStaysForeignAfterOurInstanceJoinsItsCard(t *testing.T) {
	const bdf = "0000:3f:00.0"
	gpustack := gpu.ComputeProcess{PID: 41235, Name: "python3", PCIeBDF: "00000000:3f:00.0", MemoryBytes: 67089334272}
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{gpustack}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober

	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 {
		t.Fatalf("before our instance = %+v, want the container's process", got)
	}

	// A partition of ours starts on the card; the container grows.
	ours := accelerator.CardIdent("GPU-3f", bdf)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Idents: []string{ours}} }
	grown := gpustack
	grown.MemoryBytes = 70 << 30
	prober.procs = []gpu.ComputeProcess{grown, {PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30}}
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	got := cards[0].ForeignProcesses
	if len(got) != 1 || got[0].PID != 41235 || got[0].Name != "python3" || got[0].MemoryBytes != 70<<30 {
		t.Fatalf("with our instance on the card = %+v, want only the container's process at its current 70 GiB", got)
	}

	// A process that appears on the card after ours is not examined: it
	// may be one of our engine's, and calling it foreign would stop the
	// card the platform is using (the coarse rule, unchanged).
	prober.procs = append(prober.procs, gpu.ComputeProcess{PID: 7002, Name: "python3", PCIeBDF: bdf, MemoryBytes: 1 << 30})
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 {
		t.Fatalf("after a new process on our card = %+v, want only the known foreign one", got)
	}

	// Not knowing is not knowing the card is free: a round that cannot ask
	// the tool, or whose card probe failed, says what the last one found.
	for _, round := range []struct {
		name    string
		probeOK bool
		err     error
	}{
		{"the process list timed out", true, errors.New("nvidia-smi: no answer within 10s")},
		{"the card probe failed", false, nil},
	} {
		prober.err = round.err
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, round.probeOK)
		if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 || got[0].MemoryBytes != 70<<30 {
			t.Fatalf("%s: our card = %+v, want the last foreign list restated", round.name, got)
		}
	}
	prober.err = nil

	// The container exits. The card is let go only after the release
	// rounds, as any other card would be.
	prober.procs = []gpu.ComputeProcess{{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30}}
	for i := 1; i < occupancyReleaseRounds; i++ {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 {
			t.Fatalf("free reading %d let our card go early: %+v", i, got)
		}
	}
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("after the container left = %+v, want the card free of foreign use", got)
	}
	// Nor does it come back: our engine's process is not the container's.
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("a later round brought back %+v", got)
	}
}

// A process ID the tool reuses for a different executable is not the
// process that was known to be foreign.
func TestAReusedProcessIDOnOurCardIsNotTheForeignProcess(t *testing.T) {
	const bdf = "0000:3f:00.0"
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 41235, Name: "python3", PCIeBDF: bdf, MemoryBytes: 8 << 30}}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober
	n.CardHealthFindings(context.Background(), twoOwnedCards(), true)

	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-3f", bdf)}}
	}
	prober.procs = []gpu.ComputeProcess{{PID: 41235, Name: "vllm", PCIeBDF: bdf, MemoryBytes: 8 << 30}}
	var cards []proto.GPUInfo
	for range occupancyReleaseRounds {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
	}
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("our card = %+v, want the reused PID not taken for the foreign process", got)
	}
}

// An agent restart (an upgrade) must not forget them either: the restarted
// agent finds our instance on the card and does not examine it, so what it
// knew is kept beside its state and read back.
func TestARestartedAgentStillKnowsTheForeignProcessOnOurCard(t *testing.T) {
	const bdf = "0000:3f:00.0"
	procs := []gpu.ComputeProcess{{PID: 41235, Name: "python3", PCIeBDF: bdf, MemoryBytes: 67089334272}}
	modelsDir := filepath.Join(t.TempDir(), "models")
	start := func() *NodeRuntime {
		n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, modelsDir, "none",
			&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}, procs: procs}, nil)
		n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
		return n
	}
	first := start()
	first.CardHealthFindings(context.Background(), twoOwnedCards(), true)

	// Our partition starts beside the container; then the agent restarts.
	procs = append(procs, gpu.ComputeProcess{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30})
	restarted := start()
	restarted.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-3f", bdf)}}
	}
	cards := twoOwnedCards()
	restarted.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 {
		t.Fatalf("after a restart our card = %+v, want the container's process still named", got)
	}
}

// A state file that cannot be read is no knowledge, not a failure.
func TestAnUnreadableOccupancyFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, cardOccupancyFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var o cardOccupancy
	o.load(path)
	if len(o.last) != 0 {
		t.Fatalf("last = %+v, want nothing from a broken file", o.last)
	}
}
