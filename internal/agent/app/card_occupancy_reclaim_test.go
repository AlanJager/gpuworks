package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The worker of GWKS-432: a headless rank's tensor-parallel worker on an
// RTX 3090, still listed after its deployment was deleted.
var leavingWorker = gpu.ComputeProcess{PID: 1623560, Name: "VLLM::Worker_TP1", PCIeBDF: "00000000:3f:00.0", MemoryBytes: 24480 << 20}

// reclaimNode is a node whose first card serves one whole-card instance of
// ours, read once with its engine listed.
func reclaimNode(t *testing.T, held engine.HeldCards) (*NodeRuntime, *occupiedProber) {
	t.Helper()
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{leavingWorker}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober
	n.heldCards = func() engine.HeldCards { return held }
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("our own serving card = %+v, want nothing foreign", got)
	}
	return n, prober
}

func readCards(n *NodeRuntime) []proto.GPUInfo {
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	return cards
}

func ourFirstCard() engine.HeldCards {
	return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-3f", "0000:3f:00.0")}}
}

// A deployment is deleted and its container confirmed gone, but the vendor
// tool still lists the engine: killed, it is letting go of 24 GiB. The card
// must not take work while that memory is held - and it is ours on its way
// out, not somebody's job that comes and goes, so the reading that finds it
// gone returns the card. Before, the card stayed reported for three more
// readings, three minutes in which nothing could be placed on it.
func TestACardIsLetGoAsSoonAsOurLeavingEngineIsGone(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())

	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	for round := 1; round <= 2; round++ {
		got := readCards(n)[0].ForeignProcesses
		if len(got) != 1 || got[0].PID != leavingWorker.PID || got[0].MemoryBytes != leavingWorker.MemoryBytes {
			t.Fatalf("reading %d with the engine still on the card = %+v, want it reported: the memory is held", round, got)
		}
	}

	prober.procs = nil
	cards := readCards(n)
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("the reading that found our engine gone = %+v, want the card let go at once", got)
	}
	if n.occupancy.foreign(accelerator.CardIdent("GPU-3f", "0000:3f:00.0")) {
		t.Fatal("the card is still remembered as used outside the platform")
	}
	if got := cards[1].ForeignProcesses; len(got) != 0 {
		t.Fatalf("the other card = %+v, want it untouched", got)
	}
}

// The release rounds stay for anybody else: a job that somebody started on
// the card our instance has just left is theirs, whether it showed up beside
// our leaving engine or under its PID with another name.
func TestSomebodyElsesProcessOnALeftCardKeepsItsReleaseRounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after []gpu.ComputeProcess
		want  int
	}{
		{"a job beside our leaving engine", []gpu.ComputeProcess{leavingWorker, {PID: 4242, Name: "python3", PCIeBDF: "0000:3f:00.0", MemoryBytes: 2 << 30}}, 2},
		{"our engine's PID reused by another program", []gpu.ComputeProcess{{PID: leavingWorker.PID, Name: "python3", PCIeBDF: "0000:3f:00.0", MemoryBytes: 2 << 30}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, prober := reclaimNode(t, ourFirstCard())
			n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
			prober.procs = tc.after
			if got := readCards(n)[0].ForeignProcesses; len(got) != tc.want {
				t.Fatalf("after our instance left = %+v, want %d processes reported", got, tc.want)
			}
			prober.procs = nil
			for i := 1; i < occupancyReleaseRounds; i++ {
				if got := readCards(n)[0].ForeignProcesses; len(got) != tc.want {
					t.Fatalf("free reading %d let the card go early: %+v", i, got)
				}
			}
			if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
				t.Fatalf("after the release rounds = %+v, want the card free", got)
			}
		})
	}
}

// A reading that could not ask the tool says what the last one found, for a
// leaving engine as for anybody: not knowing is not knowing it is gone. The
// next reading that does ask and finds it gone lets the card go.
func TestALeavingEngineStaysReportedWhileTheToolDoesNotAnswer(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	if got := readCards(n)[0].ForeignProcesses; len(got) != 1 {
		t.Fatalf("after our instance left = %+v, want the engine reported", got)
	}

	prober.procs, prober.err = nil, errors.New("nvidia-smi: no answer within 10s")
	if got := readCards(n)[0].ForeignProcesses; len(got) != 1 || got[0].PID != leavingWorker.PID {
		t.Fatalf("while the tool did not answer = %+v, want the last answer restated", got)
	}
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, false)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != leavingWorker.PID {
		t.Fatalf("while the card probe failed = %+v, want the last answer restated", got)
	}

	prober.err = nil
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("once the tool answered without the engine = %+v, want the card let go", got)
	}
}

// listingProber runs during before it answers, the way an instance's
// teardown finishes while nvidia-smi is still enumerating.
type listingProber struct {
	*occupiedProber
	during func()
}

func (p *listingProber) ComputeProcesses(ctx context.Context) ([]gpu.ComputeProcess, error) {
	if p.during != nil {
		p.during()
	}
	return p.occupiedProber.ComputeProcesses(ctx)
}

// The list names what was on the cards when the tool read them; which cards
// are ours was asked after it answered. An instance torn down in between is
// in the list and no longer ours: its engine was reported as somebody
// else's, though the container - and the process - were already gone.
func TestAnInstanceTornDownWhileTheListIsReadIsNotSomebodyElses(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())
	held := ourFirstCard()
	n.heldCards = func() engine.HeldCards { return held }
	n.prober = &listingProber{occupiedProber: prober, during: func() { held = engine.HeldCards{} }}

	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("the reading the teardown finished in = %+v, want nothing: the list predates it", got)
	}
	// The next reading examines the card for real.
	n.prober.(*listingProber).during = nil
	prober.procs = nil
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("the next reading of the free card = %+v, want nothing", got)
	}

	// Had the engine in fact outlived its container, that reading says so.
	n, prober = reclaimNode(t, ourFirstCard())
	held = ourFirstCard()
	n.heldCards = func() engine.HeldCards { return held }
	n.prober = &listingProber{occupiedProber: prober, during: func() { held = engine.HeldCards{} }}
	readCards(n)
	if got := readCards(n)[0].ForeignProcesses; len(got) != 1 || got[0].PID != leavingWorker.PID {
		t.Fatalf("an engine still listed a reading later = %+v, want it reported", got)
	}
}

// A partition's engine and a sleeper's are known by PID while their
// instances are on the card (own); they are the leaving engines of such a
// card. Real foreign use beside them keeps the card's release rounds.
func TestALeavingPartitionOrSleeperIsOurEngineToo(t *testing.T) {
	ident := accelerator.CardIdent("GPU-3f", "0000:3f:00.0")
	t.Run("partition", func(t *testing.T) {
		n, prober := reclaimNode(t, engine.HeldCards{Idents: []string{ident}, Partitions: []string{ident}})
		n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
		if got := readCards(n)[0].ForeignProcesses; len(got) != 1 {
			t.Fatalf("after the partition left = %+v, want its engine reported while listed", got)
		}
		prober.procs = nil
		if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("once the partition's engine was gone = %+v, want the card let go", got)
		}
	})
	t.Run("sleeper", func(t *testing.T) {
		n, prober := reclaimNode(t, ourFirstCard())
		n.sleepingCards = func() map[string]map[int]bool {
			return map[string]map[int]bool{ident: {leavingWorker.PID: true}}
		}
		if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("the sleeper's card = %+v, want nothing foreign", got)
		}
		n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
		n.sleepingCards = func() map[string]map[int]bool { return nil }
		if got := readCards(n)[0].ForeignProcesses; len(got) != 1 {
			t.Fatalf("after the sleeper left = %+v, want its engine reported while listed", got)
		}
		prober.procs = nil
		if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("once the sleeper's engine was gone = %+v, want the card let go", got)
		}
	})
	t.Run("partition beside a foreign job", func(t *testing.T) {
		outside := gpu.ComputeProcess{PID: 41235, Name: "python3", PCIeBDF: "0000:3f:00.0", MemoryBytes: 8 << 30}
		prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
			procs: []gpu.ComputeProcess{outside}}
		n := occupancyNode(t, twoOwnedCards(), nil, nil)
		n.prober = prober
		readCards(n)
		n.heldCards = func() engine.HeldCards {
			return engine.HeldCards{Idents: []string{ident}, Partitions: []string{ident}}
		}
		prober.procs = []gpu.ComputeProcess{outside, leavingWorker}
		if got := readCards(n)[0].ForeignProcesses; len(got) != 1 || got[0].PID != outside.PID {
			t.Fatalf("with our partition beside it = %+v, want only the outside job", got)
		}
		n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
		prober.procs = nil
		for i := 1; i < occupancyReleaseRounds; i++ {
			if got := readCards(n)[0].ForeignProcesses; len(got) != 1 || got[0].PID != outside.PID {
				t.Fatalf("free reading %d let a card somebody else held go early: %+v", i, got)
			}
		}
		if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("after the release rounds = %+v, want the card free", got)
		}
	})
}

// datedProber answers with a list produced age ago, the way a slow vendor
// tool is answered from its last result (gpu.QueryVendorCLI records when).
// took is how long the run behind it ran: age is since it completed.
type datedProber struct {
	*occupiedProber
	age  time.Duration
	took time.Duration
}

func (p *datedProber) ComputeProcesses(ctx context.Context) ([]gpu.ComputeProcess, error) {
	completed := time.Now().Add(-p.age)
	gpu.RecordResultSpan(ctx, completed.Add(-p.took), completed)
	return p.occupiedProber.ComputeProcesses(ctx)
}

// On both nodes of GWKS-432 nvidia-smi took 12 to 31 seconds a query, so the
// agent was served its last list: one produced before the deployment was
// deleted, naming an engine whose container was gone by the time it was
// read. Such a list says nothing about a card our instances have left
// since; the first list produced after they were seen gone does.
func TestAListOlderThanTheTeardownDoesNotNameOurEngineAsSomebodyElses(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())
	stale := &datedProber{occupiedProber: prober, age: 40 * time.Second}
	n.prober = stale
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	for round := 1; round <= 2; round++ {
		if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("reading %d of a list older than the teardown = %+v, want nothing: the engine is gone", round, got)
		}
	}

	// A list produced since is believed, whatever it says.
	stale.age = 0
	if got := readCards(n)[0].ForeignProcesses; len(got) != 1 || got[0].PID != leavingWorker.PID {
		t.Fatalf("a list produced after the teardown that still names the engine = %+v, want it reported", got)
	}
	prober.procs = nil
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("once it is gone = %+v, want the card let go", got)
	}
}

// An old list is still what there is to go on for a card none of ours was
// on: somebody's job there is reported from it, as before.
func TestAnOldListStillReportsACardOursWereNeverOn(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())
	prober.procs = append(prober.procs, gpu.ComputeProcess{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0", MemoryBytes: 8 << 30})
	n.prober = &datedProber{occupiedProber: prober, age: 40 * time.Second}
	cards := readCards(n)
	if got := cards[1].ForeignProcesses; len(got) != 1 || got[0].PID != 4242 {
		t.Fatalf("the other card = %+v, want somebody's job reported from the old list", got)
	}
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("our card = %+v, want nothing foreign", got)
	}
}

// The runtime check's own process is left out for a moment after the check
// (smokeOccupancyGrace) because a listing is read a little after it was
// produced. A list a slow tool produced during the check and served a
// minute later still shows that process: the moment counts from when the
// list was produced, not from when it was read.
func TestAnOldListStillShowingTheRuntimeChecksProcessIsNotForeign(t *testing.T) {
	now := time.Now()
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 9001, Name: "python3", PCIeBDF: "0000:3f:00.0", MemoryBytes: 110 << 20}}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	dated := &datedProber{occupiedProber: prober, age: time.Minute}
	n.prober = dated
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{Now: func() time.Time { return now }})
	n.cardSmoke.Begin(accelerator.CardIdent("GPU-3f", "0000:3f:00.0"))()

	now = now.Add(time.Minute)
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("a list produced during the check, read a minute later = %+v, want the check's process left out", got)
	}
	// A list produced well after the check is believed.
	dated.age = 0
	if got := readCards(n)[0].ForeignProcesses; len(got) != 1 {
		t.Fatalf("a list produced a minute after the check = %+v, want the process reported", got)
	}
}

// Reproduced on the nodes of GWKS-432: the worker of a deleted deployment
// was gone from the host by 00:09:20 and reported as somebody else's at
// 00:09:41. A slow tool spends tens of seconds on one answer and may have
// read the cards at any point of that, so an answer that completed after
// the card was seen without our instance, from a run that started before,
// can still show its engine. The list is dated by when its run started.
func TestAListWhoseRunStartedBeforeTheTeardownDoesNotNameOurEngine(t *testing.T) {
	n, prober := reclaimNode(t, ourFirstCard())
	slow := &datedProber{occupiedProber: prober, took: 20 * time.Second}
	n.prober = slow
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	// The first reading without our instance: a fresh-looking answer whose
	// run started 20 s ago, while the engine was still there.
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("an answer from a run that started before the teardown = %+v, want nothing", got)
	}
	// The next one completed after that reading and started before it.
	time.Sleep(20 * time.Millisecond)
	slow.age, slow.took = 5*time.Millisecond, 20*time.Second
	if got := readCards(n)[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("an answer completed since, from a run that started before = %+v, want nothing", got)
	}
	// A run that started after the card was seen without our instance is
	// believed: here the engine is gone, and the card stays free.
	slow.took = 0
	prober.procs = nil
	cards := readCards(n)
	if got := cards[0].ForeignProcesses; len(got) != 0 {
		t.Fatalf("the first list from a run started since = %+v, want the card free", got)
	}
	if n.occupancy.foreign(accelerator.CardIdent("GPU-3f", "0000:3f:00.0")) {
		t.Fatal("the card is remembered as used outside the platform")
	}
}
