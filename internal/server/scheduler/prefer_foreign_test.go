package scheduler

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// cleanCard is a free 140 GiB card with the partition backend, nobody else
// on it; degraded marks it at warning level.
func cleanCard(index int, degraded bool) GPU {
	g := freeH20(index)
	g.Degraded = degraded
	return g
}

// enabledNode is the reporter's node whose held cards an operator enabled
// scheduling on, so the scheduler may choose them.
func enabledNode(gpus ...GPU) NodeSnapshot {
	for i, g := range gpus {
		if g.HeldOutsidePlatform {
			gpus[i] = enabled(g)
		}
	}
	return nvidiaNode(gpus...)
}

func sharedCard(index int, degraded bool) GPU {
	g := reporterCard(index)
	g.Degraded = degraded
	return g
}

// The tiers relax the table in lexicographic order (D1): healthy and clean,
// then degraded but clean, then healthy but shared, then anything. Each row
// offers the cards and says which the partition goes on.
func TestPreferenceTiersFollowTheTableOrder(t *testing.T) {
	full := func(g GPU) GPU {
		g.AllocatedBytes, g.WholeGPUAllocated = g.MemoryTotal, true
		return g
	}
	for _, tc := range []struct {
		name    string
		cards   []GPU
		want    int
		reasons []string
	}{
		{"healthy clean first", []GPU{sharedCard(0, false), cleanCard(1, true), cleanCard(2, false)}, 2, nil},
		{"degraded clean before healthy shared", []GPU{sharedCard(0, false), cleanCard(1, true)}, 1, []string{ReasonDegradedGPUsUsed}},
		{"healthy shared before degraded shared", []GPU{sharedCard(0, true), sharedCard(1, false)}, 1, []string{ReasonForeignSharedGPUsUsed}},
		{"degraded shared last", []GPU{full(cleanCard(0, false)), sharedCard(1, true)}, 1, []string{ReasonForeignSharedGPUsUsed, ReasonDegradedGPUsUsed}},
		// Card 0 is degraded, clean and full; card 1 healthy and shared;
		// card 2 degraded and shared: relaxing "shared" keeps "degraded"
		// avoided, so card 1.
		{"three cards", []GPU{full(cleanCard(0, true)), sharedCard(1, false), sharedCard(2, true)}, 1, []string{ReasonForeignSharedGPUsUsed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Schedule(automatic(16), enabledNode(tc.cards...))
			if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{tc.want}) {
				t.Fatalf("plan = %+v %+v, want GPU %d", res.Plan, res.Message, tc.want)
			}
			var got []string
			for _, concession := range res.Plan.Concessions {
				got = append(got, concession.Code)
			}
			if !reflect.DeepEqual(got, tc.reasons) {
				t.Fatalf("concessions = %v, want %v", got, tc.reasons)
			}
			for _, code := range tc.reasons {
				if hasReason(res.Plan, code) == nil {
					t.Fatalf("plan reasons %+v lack %s", res.Plan.Reasons, code)
				}
			}
		})
	}
}

// Enabled, a held card still loses to a clean card that fits; the plan on a shared card says
// how many and how much the processes outside hold, and marks the
// concession as worth looking at other nodes for.
func TestSharingIsUsedOnlyWhenNoCleanCardFits(t *testing.T) {
	res := Schedule(automatic(16), enabledNode(reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan.GPUIndexes[0] != 1 || len(res.Plan.Concessions) != 0 {
		t.Fatalf("plan = %+v, want the clean GPU 1 with no concession", res.Plan)
	}
	busy := freeH20(1)
	busy.AllocatedBytes, busy.WholeGPUAllocated = busy.MemoryTotal, true
	res = Schedule(automatic(16), enabledNode(reporterCard(0), busy))
	if !res.OK || res.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("plan = %+v %+v, want the shared GPU 0", res.Plan, res.Message)
	}
	reason := hasReason(res.Plan, ReasonForeignSharedGPUsUsed)
	if reason == nil || !reflect.DeepEqual(reason.Params["gpus"], []int{0}) || reason.Params["count"] != 1 || reason.Params["foreign_gib"] != gib(reporterForeignBytes) {
		t.Fatalf("reason = %+v", reason)
	}
	if !reflect.DeepEqual(res.Plan.Concessions, []Concession{{Code: ReasonForeignSharedGPUsUsed, PoolWide: true}}) {
		t.Fatalf("concessions = %+v", res.Plan.Concessions)
	}
}

// A pin to a shared card nobody enabled lands there with the concession
// stated, and so does one the scheduler chose because an operator enabled it.
func TestPinnedAndEnabledCardsAndConcessions(t *testing.T) {
	res := Schedule(reporterPartition(16), nvidiaNode(reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan.GPUIndexes[0] != 0 || hasReason(res.Plan, ReasonForeignSharedGPUsUsed) == nil {
		t.Fatalf("pinned plan = %+v %+v", res.Plan, res.Message)
	}
	res = Schedule(automatic(16), enabledNode(reporterCard(0)))
	if !res.OK || !reflect.DeepEqual(res.Plan.Concessions, []Concession{{Code: ReasonForeignSharedGPUsUsed, PoolWide: true}}) {
		t.Fatalf("enabled card plan = %+v %+v, want the sharing concession", res.Plan, res.Message)
	}
	// Holders whose memory is not known have no figure to state: the card
	// an operator enabled is used without the concession.
	unknown := enabled(reporterCard(0))
	unknown.ForeignShareable, unknown.ForeignBytes = false, 0
	res = Schedule(automatic(16), nvidiaNode(unknown))
	if !res.OK || len(res.Plan.Concessions) != 0 {
		t.Fatalf("enabled card of unknown holders = %+v %+v, want placed with no concession", res.Plan, res.Message)
	}
}

// The deploy form's card list is what an operator picks from, and picking
// is consent: a shared card is offered with a note though nobody enabled
// scheduling on it; a deployment that sleeps is not offered it.
func TestGPUCandidatesNoteASharedCard(t *testing.T) {
	node := nvidiaNode(reporterCard(0), freeH20(1))
	got := GPUCandidates(reporterPartition(16), node)
	if !got[0].Available || got[0].Note == nil || got[0].Note.Code != ReasonForeignSharedCard || got[0].Note.Params["foreign_gib"] != gib(reporterForeignBytes) {
		t.Fatalf("shared card = %+v note %+v", got[0], got[0].Note)
	}
	if !got[1].Available || got[1].Note != nil {
		t.Fatalf("clean card = %+v", got[1])
	}
	sleepy := reporterPartition(16)
	sleepy.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	got = GPUCandidates(sleepy, node)
	if got[0].Available || got[0].Reason == nil || got[0].Reason.Code != ReasonHeldOutsidePlatformSleep {
		t.Fatalf("sleepable on shared card = %+v", got[0])
	}
}
