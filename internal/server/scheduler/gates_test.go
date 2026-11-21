package scheduler

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// enabled is a held card an operator enabled scheduling on: it takes work
// whatever the platform judged (store.GPU.CanTakeWork).
func enabled(g GPU) GPU {
	g.Schedulable = true
	return g
}

// automatic is the reporter's partition with the scheduler choosing cards.
func automatic(limitGiB int64) Request {
	req := reporterPartition(limitGiB)
	req.AllowedGPUIndexes, req.Consent = nil, Consent{}
	return req
}

// Automatic placement stays off held cards nobody enabled scheduling on,
// and the refusal names those cards on their node; enabling them places the
// deployment.
func TestHeldCardsAreWithheldUntilAnOperatorEnablesThem(t *testing.T) {
	res := Schedule(automatic(16), nvidiaNode(reporterCard(0), reporterCard(1)))
	if res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("not enabled = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformNotAllowed)
	}
	params := res.Message.Params
	if _, named := params["setting"]; named || params["count"] != 2 ||
		!reflect.DeepEqual(params["gpus"], []int{0, 1}) || params["node"] != "h20" {
		t.Fatalf("params = %+v", params)
	}
	res = Schedule(automatic(16), nvidiaNode(reporterCard(0), enabled(reporterCard(1))))
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{1}) {
		t.Fatalf("one card enabled = ok %v %+v plan %+v, want GPU 1", res.OK, res.Message, res.Plan)
	}
}

// A node with a clean card that is busy and three held ones is the common
// case: the refusal still points at the held cards, because enabling them
// would place the deployment - not at the busy card.
func TestABusyCleanCardStillNamesTheHeldCards(t *testing.T) {
	busy := freeH20(0)
	busy.AllocatedBytes, busy.WholeGPUAllocated = busy.MemoryTotal, true
	res := Schedule(automatic(16), nvidiaNode(busy, reporterCard(1), reporterCard(2), reporterCard(3)))
	if res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("refusal = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformNotAllowed)
	}
	if res.Message.Params["count"] != 3 || !reflect.DeepEqual(res.Message.Params["gpus"], []int{1, 2, 3}) {
		t.Fatalf("params = %+v", res.Message.Params)
	}
	// Enabling them would not help: the ordinary reason stands.
	huge := automatic(130)
	if res := Schedule(huge, nvidiaNode(busy, reporterCard(1))); res.OK || res.Message.Code == ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("a request too large for the held card = %+v, want the memory reason", res.Message)
	}
}

// Picking the cards is consent: a manual placement on a held card nobody
// enabled, that fits by memory, is admitted.
func TestChosenCardsConsentToSharing(t *testing.T) {
	res := Schedule(reporterPartition(16), nvidiaNode(reporterCard(0), freeH20(1)))
	if !res.OK || res.Plan.GPUIndexes[0] != 0 {
		t.Fatalf("chosen held card = ok %v %+v plan %+v, want GPU 0", res.OK, res.Message, res.Plan)
	}
	// A pin without consent - a plan echoed back, a probe - is refused.
	unconsented := reporterPartition(16)
	unconsented.Consent = Consent{}
	if res := Schedule(unconsented, nvidiaNode(reporterCard(0), freeH20(1))); res.OK || res.Message.Code != ReasonHeldOutsidePlatformNotAllowed {
		t.Fatalf("pinned without consent = ok %v %+v", res.OK, res.Message)
	}
}

// Choosing a card does not stretch what may share it: not whole-card work,
// not holders of unknown memory, not a deployment that sleeps (whose gate
// comes first and never yields).
func TestChoosingACardDoesNotOverrideWhatCannotShare(t *testing.T) {
	whole := reporterPartition(16)
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	if res := Schedule(whole, nvidiaNode(reporterCard(0))); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("whole card = ok %v %+v", res.OK, res.Message)
	}
	unknown := reporterCard(0)
	unknown.ForeignShareable, unknown.ForeignBytes = false, 0
	if res := Schedule(reporterPartition(16), nvidiaNode(unknown)); res.OK || res.Message.Code != ReasonGPUsHeldOutsidePlatform {
		t.Fatalf("unknown holder = ok %v %+v", res.OK, res.Message)
	}
	sleepy := reporterPartition(16)
	sleepy.Features = []accelerator.Feature{accelerator.FeatureEngineSleep}
	if res := Schedule(sleepy, nvidiaNode(reporterCard(0))); res.OK || res.Message.Code != ReasonHeldOutsidePlatformSleep {
		t.Fatalf("sleepable = ok %v %+v, want %s", res.OK, res.Message, ReasonHeldOutsidePlatformSleep)
	}
}

// Enabling scheduling on a held card is the operator's decision about the
// card itself: it takes work the scheduler chooses, whole-card work
// included, and no gate withholds it.
func TestAnEnabledHeldCardTakesWorkTheSchedulerChooses(t *testing.T) {
	card := enabled(reporterCard(0))
	if res := Schedule(automatic(16), nvidiaNode(card)); !res.OK {
		t.Fatalf("enabled card = %+v, want placed", res.Message)
	}
	if resolved := resolveGates(automatic(16), nvidiaNode(card)); resolved.GPUs[0].withheld != 0 {
		t.Fatalf("enabled card withheld by gate %d", resolved.GPUs[0].withheld)
	}
	whole := automatic(16)
	whole.PerGPULimitBytes, whole.PartitionBackend = 0, ""
	if res := Schedule(whole, nvidiaNode(card)); !res.OK {
		t.Fatalf("whole card on an enabled card = %+v, want placed", res.Message)
	}
}

// A clean card is used before an enabled held one, and a plan that has to
// use the held one says so.
func TestAnEnabledHeldCardIsUsedAfterCleanCards(t *testing.T) {
	res := Schedule(automatic(16), nvidiaNode(enabled(reporterCard(0)), freeH20(1)))
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{1}) {
		t.Fatalf("plan = ok %v %+v %+v, want the clean GPU 1", res.OK, res.Message, res.Plan)
	}
	res = Schedule(automatic(16), nvidiaNode(enabled(reporterCard(0))))
	if !res.OK {
		t.Fatalf("only the enabled card = %+v", res.Message)
	}
	var noted bool
	for _, reason := range res.Plan.Reasons {
		noted = noted || reason.Code == ReasonForeignSharedGPUsUsed
	}
	if !noted {
		t.Fatalf("plan reasons = %+v, want %s", res.Plan.Reasons, ReasonForeignSharedGPUsUsed)
	}
}

// The gates never change what the snapshot says the card is.
func TestGatesLeaveTheCardsFactsAlone(t *testing.T) {
	node := nvidiaNode(reporterCard(0))
	resolved := resolveGates(automatic(16), node)
	if !resolved.GPUs[0].ForeignShareable || resolved.GPUs[0].withheld == 0 {
		t.Fatalf("resolved card = %+v, want shareable and withheld", resolved.GPUs[0])
	}
	if node.GPUs[0].withheld != 0 {
		t.Fatal("resolveGates changed the caller's snapshot")
	}
}

// A future gate is a table entry, and it may withhold a card that takes
// work: the mechanism does not assume held cards.
func TestAFurtherGateCanWithholdASchedulableCard(t *testing.T) {
	saved := cardGates
	t.Cleanup(func() { cardGates = saved })
	cardGates = append(append([]cardGate(nil), saved...), cardGate{
		name:    "test_odd_cards",
		applies: func(g GPU, _ Request) bool { return g.Index%2 == 1 },
		refusal: func(Request, string, []GPU) Reason { return Reason{Code: "test_odd_cards"} },
	})
	whole := Request{WeightBytes: giB, HeadCount: 32, GPUMemUtil: 0.9}
	res := Schedule(whole, NodeSnapshot{UUID: "n", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(1, 48, true, 0), gpu(3, 48, true, 0)}})
	if res.OK || res.Message.Code != "test_odd_cards" {
		t.Fatalf("odd cards only = ok %v %+v", res.OK, res.Message)
	}
	res = Schedule(whole, NodeSnapshot{UUID: "n", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(1, 48, true, 0), gpu(2, 48, true, 0)}})
	if !res.OK || res.Plan.GPUIndexes[0] != 2 {
		t.Fatalf("plan = %+v %+v, want GPU 2", res.Plan, res.Message)
	}
}
