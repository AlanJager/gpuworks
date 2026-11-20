package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What the Ascend check answers for a die that will not be taken, as
// gpu.AscendRuntimeSmokeOn words it from the script's
// "ASCEND_ACL_SMOKE_DIE_FAILED: aclrtSetDevice=507033".
const ascendDieNotTaken = "Ascend ACL runtime smoke failed: aclrtSetDevice=507033"

// sharedDieNode is an Ascend node with two 910B4 dies whose runtime check
// answers as told, and whose instances are whatever the test says they are.
type sharedDieNode struct {
	*NodeRuntime
	mu      sync.Mutex
	asked   []string
	failing map[string]bool
	// placed is every instance the node was told to run, by the die it is
	// on; serving are the ones among them that serve.
	placed  map[string]string
	serving map[string]bool
	// servingByIndex, when set, is what the other instances serve on as an
	// engine bound by device index only states it.
	servingByIndex []int
}

func ascend910B4Dies() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "ascend-0", Vendor: "ascend", Name: "910B4", PCIeBDF: "0000:c1:00.0", MemoryTotalBytes: 32 << 30, Healthy: true, HealthStatus: accelerator.HealthOK},
		{Index: 1, UUID: "ascend-1", Vendor: "ascend", Name: "910B4", PCIeBDF: "0000:c2:00.0", MemoryTotalBytes: 32 << 30, Healthy: true, HealthStatus: accelerator.HealthOK},
	}
}

func newSharedDieNode(t *testing.T) *sharedDieNode {
	t.Helper()
	s := &sharedDieNode{failing: map[string]bool{}, placed: map[string]string{}, serving: map[string]bool{}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "ascend", GPUs: ascend910B4Dies()}, nil)
	s.NodeRuntime = n
	// As the engine answers: every instance told to run holds its die, the
	// failed one too; only the ones that serve are serving.
	n.heldCards = func() engine.HeldCards {
		s.mu.Lock()
		defer s.mu.Unlock()
		var held engine.HeldCards
		for _, die := range s.placed {
			held.Idents = append(held.Idents, die)
			held.Partitions = append(held.Partitions, die)
		}
		return held
	}
	n.instanceCardIdents = func(uuid string) ([]string, engine.HeldCards) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.servingByIndex != nil {
			return []string{s.placed[uuid]}, engine.HeldCards{Indexes: s.servingByIndex}
		}
		var serving engine.HeldCards
		for other, die := range s.placed {
			if other != uuid && s.serving[other] {
				serving.Idents = append(serving.Idents, die)
			}
		}
		if die, ok := s.placed[uuid]; ok {
			return []string{die}, serving
		}
		return nil, serving
	}
	check := hostCheck(t, "ascend")
	answering := check
	answering.Try = func(_ context.Context, die string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.asked = append(s.asked, die)
		if s.failing[die] {
			return errors.New(ascendDieNotTaken)
		}
		return nil
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:    func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:      cardSmokeAttempt(answering),
		Busy:     func(card cardsmoke.Card) bool { ours, _, _ := n.cardsInUse(); return ours[card.Ident] > 0 },
		Interval: time.Hour,
	})
	n.cardSmokeCtx = context.Background()
	return s
}

func (s *sharedDieNode) place(uuid, die string, serving bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placed[uuid], s.serving[uuid] = die, serving
}

func (s *sharedDieNode) askedDies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// neverAsked holds for long enough that a check started off the reporting
// path would have reached the die.
func (s *sharedDieNode) neverAsked(t *testing.T, why string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if asked := s.askedDies(); len(asked) != 0 {
			t.Fatalf("asked dies %v: %s", asked, why)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *sharedDieNode) waitAsked(t *testing.T, ident string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		outcomes := s.cardSmoke.Outcomes()
		if _, answered := outcomes[ident]; answered {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("card %s was never asked: %+v", ident, outcomes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *sharedDieNode) reported(t *testing.T) []proto.GPUInfo {
	t.Helper()
	cards := ascend910B4Dies()
	s.CardHealthFindings(context.Background(), cards, true)
	return cards
}

func runtimeCheckOf(t *testing.T, card proto.GPUInfo) proto.CardCheck {
	t.Helper()
	for _, c := range card.Checks {
		if c.ID == accelerator.CheckRuntime {
			return c
		}
	}
	t.Fatalf("card %d reports no runtime check: %+v", card.Index, card.Checks)
	return proto.CardCheck{}
}

// A die two partitions are serving on is not asked when a third partition
// fails to start on it, even though the die would answer the check the way it
// answered that partition. The partitions serving are the answer; the failed
// start's own error is what explains it, and the die stays schedulable.
func TestAFailedStartBesideServingInstancesLeavesTheirCardAlone(t *testing.T) {
	s := newSharedDieNode(t)
	die0 := accelerator.CardIdent("ascend-0", "0000:c1:00.0")
	s.place("slice-8g-a", die0, true)
	s.place("slice-8g-b", die0, true)
	s.place("slice-1g", die0, false)
	// The die answers a new context the way it answered the failed start.
	s.failing["0"] = true

	s.reportInstance(proto.InstanceStatus{UUID: "slice-1g", State: proto.InstFailed,
		Message: "NPU function error: c10_npu::SetDevice(device), error code is 507033"})
	s.neverAsked(t, "a die other instances are serving on was asked after a failed start beside them")

	cards := s.reported(t)
	if cards[0].HealthCode != "" || !cards[0].Healthy || cards[0].HealthStatus != accelerator.HealthOK {
		t.Fatalf("the shared die = %+v, want it healthy and schedulable", cards[0])
	}
	if c := runtimeCheckOf(t, cards[0]); c.Status == proto.CheckFail {
		t.Fatalf("the shared die's runtime check = %+v, want no failure recorded", c)
	}
	if outcomes := s.cardSmoke.Outcomes(); len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want nothing recorded against the die", outcomes)
	}
}

// The same die with nobody else serving on it is asked, and a die that will
// not be taken is reported unusable with what the runtime said: the failed
// instance holding it is not serving work.
func TestAFailedStartAloneOnItsCardStillFailsACardThatCannotBeUsed(t *testing.T) {
	s := newSharedDieNode(t)
	die0 := accelerator.CardIdent("ascend-0", "0000:c1:00.0")
	s.place("slice-1g", die0, false)
	// Another die's instance serving is no evidence about this one.
	s.place("elsewhere", accelerator.CardIdent("ascend-1", "0000:c2:00.0"), true)
	s.failing["0"] = true

	s.reportInstance(proto.InstanceStatus{UUID: "slice-1g", State: proto.InstFailed,
		Message: "NPU function error: c10_npu::SetDevice(device), error code is 507033"})
	s.waitAsked(t, die0)

	if asked := s.askedDies(); len(asked) != 1 || asked[0] != "0" {
		t.Fatalf("asked %v, want only the die the instance failed on", asked)
	}
	cards := s.reported(t)
	bad := cards[0]
	if bad.HealthCode != accelerator.HealthRuntimeUnusable || bad.Healthy || bad.HealthStatus != accelerator.HealthError {
		t.Fatalf("the die that cannot be used = %+v, want it reported unusable", bad)
	}
	if !strings.Contains(bad.HealthMessage, "aclrtSetDevice=507033") {
		t.Fatalf("the finding does not carry what the runtime said: %q", bad.HealthMessage)
	}
	if c := runtimeCheckOf(t, bad); c.Status != proto.CheckFail || c.Code != accelerator.HealthRuntimeUnusable {
		t.Fatalf("runtime check = %+v, want a failure", c)
	}
	if cards[1].HealthCode != "" || !cards[1].Healthy {
		t.Fatalf("the other die = %+v, want it untouched", cards[1])
	}
}

// A die that really fails takes its instances with it. While any of them
// still serves, a failure beside it asks nothing; the failure of the last one
// leaves nobody serving, and the die is asked and found unusable.
func TestACardThatReallyFailsIsFoundOnceNothingServesOnIt(t *testing.T) {
	s := newSharedDieNode(t)
	die0 := accelerator.CardIdent("ascend-0", "0000:c1:00.0")
	s.place("slice-a", die0, true)
	s.place("slice-b", die0, true)
	s.failing["0"] = true

	// The liveness monitor retires an instance before it reports it failed.
	s.place("slice-a", die0, false)
	s.reportInstance(proto.InstanceStatus{UUID: "slice-a", State: proto.InstFailed, Message: "container exited while running (code 1)"})
	s.neverAsked(t, "the die was asked while another instance was still serving on it")

	s.place("slice-b", die0, false)
	s.reportInstance(proto.InstanceStatus{UUID: "slice-b", State: proto.InstFailed, Message: "container exited while running (code 1)"})
	s.waitAsked(t, die0)
	if bad := s.reported(t)[0]; bad.HealthCode != accelerator.HealthRuntimeUnusable || bad.Healthy {
		t.Fatalf("the failed die = %+v, want it reported unusable once nothing serves on it", bad)
	}
}

// A die that answers after a failed start stays healthy: the start's own
// error is all there is to report.
func TestAFailedStartOnACardThatAnswersBlamesNothing(t *testing.T) {
	s := newSharedDieNode(t)
	die0 := accelerator.CardIdent("ascend-0", "0000:c1:00.0")
	s.place("slice-1g", die0, false)

	s.reportInstance(proto.InstanceStatus{UUID: "slice-1g", State: proto.InstFailed,
		Message: "NPU function error: c10_npu::SetDevice(device), error code is 507033"})
	s.waitAsked(t, die0)
	cards := s.reported(t)
	if cards[0].HealthCode != "" || !cards[0].Healthy {
		t.Fatalf("the die that answered = %+v, want it healthy", cards[0])
	}
	if c := runtimeCheckOf(t, cards[0]); c.Status != proto.CheckPass {
		t.Fatalf("runtime check = %+v, want a pass", c)
	}
}

// An instance bound by device index only still names its card, from the cards
// last probed. One whose index names no known card cannot vouch for any, so
// the failed start's card is asked.
func TestServingInstancesBoundByIndexNameTheirCards(t *testing.T) {
	s := newSharedDieNode(t)
	die0 := accelerator.CardIdent("ascend-0", "0000:c1:00.0")
	s.place("slice-1g", die0, false)
	s.servingByIndex = []int{0, 7}

	if got := servedCards(engine.HeldCards{Indexes: []int{0, 7}}, s.occupancy.identOf); len(got) != 0 {
		t.Fatalf("serving cards before any probe = %v, want none named", got)
	}
	s.occupancy.learnCards(ascend910B4Dies())
	if got := servedCards(engine.HeldCards{Indexes: []int{0, 7}}, s.occupancy.identOf); len(got) != 1 || !got[die0] {
		t.Fatalf("serving cards = %v, want only die 0 (index 7 names no card)", got)
	}
	s.reportInstance(proto.InstanceStatus{UUID: "slice-1g", State: proto.InstFailed, Message: "container exited (code 1)"})
	s.neverAsked(t, "the die an index-bound instance serves on was asked")

	// Index 7 names no card of this node, so it vouches for none.
	s.servingByIndex = []int{7}
	s.reportInstance(proto.InstanceStatus{UUID: "slice-1g", State: proto.InstFailed, Message: "container exited (code 1)"})
	s.waitAsked(t, die0)
}
