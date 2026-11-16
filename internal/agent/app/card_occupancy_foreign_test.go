package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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

	// The container's process leaves the list. While our instance is on
	// the card the agent cannot tell an exit from a restart under a new
	// PID - the new one could be our engine's - so the card keeps the last
	// figure: reading it as free would admit partitions into memory a
	// restarted container holds again.
	prober.procs = []gpu.ComputeProcess{{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30}}
	for i := 0; i < 2*occupancyReleaseRounds; i++ {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 || got[0].MemoryBytes != 70<<30 {
			t.Fatalf("reading %d without the container let our card go: %+v", i, got)
		}
	}

	// Our instance leaves: the card is examined again, and let go after the
	// release rounds, as any other card would be.
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	prober.procs = nil
	for i := 1; i < occupancyReleaseRounds; i++ {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 41235 {
			t.Fatalf("free reading %d let the card go early: %+v", i, got)
		}
	}
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("after the container left = %+v, want the card free of foreign use", got)
	}
}

// The reporter's case: the other orchestrator restarts its container while
// our partition runs beside it, and the same workload comes back under a new
// PID. The card must not read as free: the memory the workload holds again
// would be offered to the next partition.
func TestAForeignWorkloadRestartedUnderANewPIDKeepsOurCardHeld(t *testing.T) {
	const bdf = "0000:3f:00.0"
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 41235, Name: "python3", PCIeBDF: bdf, MemoryBytes: 64 << 30}}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober
	n.CardHealthFindings(context.Background(), twoOwnedCards(), true)

	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-3f", bdf)}}
	}
	prober.procs = []gpu.ComputeProcess{
		{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30},  // our partition
		{PID: 52000, Name: "python3", PCIeBDF: bdf, MemoryBytes: 64 << 30}, // the restarted container
	}
	var cards []proto.GPUInfo
	for range 2 * occupancyReleaseRounds {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
	}
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].MemoryBytes != 64<<30 {
		t.Fatalf("our card after the container restarted = %+v, want its 64 GiB still counted", got)
	}

	// Once every instance of ours there sleeps, the card is examined, and
	// the restarted container is found under its new PID.
	n.sleepingCards = func() map[string]map[int]bool {
		return map[string]map[int]bool{accelerator.CardIdent("GPU-3f", bdf): {7001: true}}
	}
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 52000 {
		t.Fatalf("examined card = %+v, want the restarted container under PID 52000", got)
	}
}

// A process ID the tool reuses for a different executable is not the
// process that was known to be foreign: what it holds is not reported in
// the foreign process's name, which keeps its last figure.
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
	prober.procs = []gpu.ComputeProcess{{PID: 41235, Name: "vllm", PCIeBDF: bdf, MemoryBytes: 20 << 30}}
	var cards []proto.GPUInfo
	for range occupancyReleaseRounds {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
	}
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].Name != "python3" || got[0].MemoryBytes != 8<<30 {
		t.Fatalf("our card = %+v, want the foreign process at its last 8 GiB, not the reused PID's 20", got)
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

// After a reboot the PIDs the file names mean nothing: our own engine may
// be given one of them. The processes it names are still restated - the
// workload outside the platform usually comes back with the node - but at
// the figure they last held and without the PID, never at what a process
// that happens to carry the same PID and name holds now. A file is from a
// previous boot when it was last written before the running boot started.
func TestAnOccupancyFileFromAnotherBootIsNotMatchedByPID(t *testing.T) {
	const bdf = "0000:3f:00.0"
	ident := accelerator.CardIdent("GPU-3f", bdf)
	modelsDir := filepath.Join(t.TempDir(), "models")
	procs := []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: bdf, MemoryBytes: 8 << 30}}
	var boot time.Time
	start := func() *NodeRuntime {
		n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, modelsDir, "none",
			&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}, procs: procs}, nil)
		n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
		// Read the state file again as a run in this boot would.
		n.occupancy.bootTime = func() time.Time { return boot }
		n.occupancy.load(n.occupancy.path)
		return n
	}
	first := start()
	first.CardHealthFindings(context.Background(), twoOwnedCards(), true)
	written, err := os.Stat(first.occupancy.path)
	if err != nil {
		t.Fatal(err)
	}

	// The node reboots after the file was written; our engine is given the
	// container's old PID.
	boot = written.ModTime().Add(time.Minute)
	procs = []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: bdf, MemoryBytes: 51 << 30}}
	rebooted := start()
	rebooted.heldCards = func() engine.HeldCards { return engine.HeldCards{Idents: []string{ident}} }
	cards := twoOwnedCards()
	rebooted.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 0 || got[0].MemoryBytes != 8<<30 {
		t.Fatalf("after a reboot our card = %+v, want the old process at its last 8 GiB without a PID, not our engine's 51", got)
	}
	// Nor does it become matchable at the next restart in the new boot.
	again := start()
	again.heldCards = rebooted.heldCards
	cards = twoOwnedCards()
	again.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 0 || got[0].MemoryBytes != 8<<30 {
		t.Fatalf("a restart after the reboot = %+v, want the old process still unmatched", got)
	}

	// Within the boot the file was written in, its PIDs still match.
	boot = written.ModTime().Add(-time.Hour)
	first.occupancy.mu.Lock()
	first.occupancy.saveLocked(nil) // rewrite the file as it was before the reboot
	first.occupancy.mu.Unlock()
	procs = []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: bdf, MemoryBytes: 9 << 30}}
	same := start()
	same.heldCards = rebooted.heldCards
	cards = twoOwnedCards()
	same.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 4242 || got[0].MemoryBytes != 9<<30 {
		t.Fatalf("after a restart in the same boot = %+v, want the live 9 GiB", got)
	}

	// Where the boot's start cannot be read, the file's PIDs are not trusted.
	boot = time.Time{}
	unknown := start()
	unknown.heldCards = rebooted.heldCards
	cards = twoOwnedCards()
	unknown.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 0 {
		t.Fatalf("with an unknown boot = %+v, want the PID dropped", got)
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

// The boot's start is the btime line of /proc/stat; anything else is not
// known.
func TestBootTimeIsReadFromProcStat(t *testing.T) {
	if got := parseBootTime("cpu  1 2 3\nbtime 1763280000\nprocesses 9\n"); !got.Equal(time.Unix(1763280000, 0)) {
		t.Fatalf("btime = %v", got)
	}
	for _, data := range []string{"", "cpu 1 2\n", "btime x\n", "btime 0\n"} {
		if got := parseBootTime(data); !got.IsZero() {
			t.Fatalf("%q = %v, want unknown", data, got)
		}
	}
}
