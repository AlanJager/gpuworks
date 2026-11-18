package store

import (
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// setHolder holds the nodes it is told to, the way a maintenance window or
// a fleet cap would, from its own in-memory state only.
type setHolder struct {
	source SchedulingSource
	code   string
	mu     sync.Mutex
	held   map[string]bool
}

func newSetHolder(source SchedulingSource, code string) *setHolder {
	return &setHolder{source: source, code: code, held: map[string]bool{}}
}

func (h *setHolder) Source() SchedulingSource { return h.source }

func (h *setHolder) Hold(f NodeHoldFacts) NodeHold {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.held[f.UUID] {
		return NodeHold{}
	}
	return NodeHold{Code: h.code, Fallback: "held by " + string(h.source), Params: map[string]any{"node": f.UUID}}
}

func (h *setHolder) set(uuid string, held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[uuid] = held
}

type holdNode struct {
	t    *testing.T
	s    *Store
	uuid string
}

func newHoldNode(t *testing.T, s *Store, uuid string) holdNode {
	t.Helper()
	if _, err := s.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
		t.Fatal(err)
	}
	return holdNode{t: t, s: s, uuid: uuid}
}

func (n holdNode) report(sys proto.SysInfo, cards ...proto.GPUInfo) Node {
	n.t.Helper()
	ok := true
	if cards == nil {
		cards = []proto.GPUInfo{{Index: 0, UUID: "GPU-" + n.uuid, Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	}
	if err := n.s.UpdateNodeStatus(n.uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: sys}); err != nil {
		n.t.Fatal(err)
	}
	return n.row()
}

func (n holdNode) row() Node {
	n.t.Helper()
	got, err := n.s.NodeByUUID(n.uuid)
	if err != nil {
		n.t.Fatal(err)
	}
	return *got
}

func wantScheduling(t *testing.T, n Node, scheduling NodeScheduling, by SchedulingSource) {
	t.Helper()
	if n.Scheduling != scheduling || n.SchedulingBy != by {
		t.Fatalf("node %s = %q by %q, want %q by %q", n.UUID, n.Scheduling, n.SchedulingBy, scheduling, by)
	}
}

var dockerOK = proto.SysInfo{RuntimeDriver: "docker"}

func TestAddSchedulingHolderValidatesTheSource(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []SchedulingSource{"", "operator", "platform", "Maint", "1maint", "a-b", "seventeen_chars_x"} {
		if err := s.AddSchedulingHolder(newSetHolder(bad, "")); err == nil {
			t.Errorf("source %q was accepted", bad)
		}
	}
	if err := s.AddSchedulingHolder(nil); err == nil {
		t.Error("a nil holder was accepted")
	}
	if err := s.AddSchedulingHolder(newSetHolder("maint", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSchedulingHolder(newSetHolder("maint", "")); !errors.Is(err, ErrHolderConflict) {
		t.Fatalf("duplicate source: err = %v, want ErrHolderConflict", err)
	}
	if err := s.AddSchedulingHolder(newSetHolder("freeze", "")); err != nil {
		t.Fatalf("a second source was refused: %v", err)
	}
}

// A held node is cordoned under its holder by the next report, even one
// whose agent cannot say anything about its runtime; it never drains.
func TestAReportCordonsAHeldNode(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("maint", "MAINT_WINDOW")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	n := newHoldNode(t, s, "held")
	wantScheduling(t, n.report(dockerOK), NodeSchedulable, SchedulingBySelf)

	h.set("held", true)
	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	wantScheduling(t, n.report(proto.SysInfo{}), NodeCordoned, "maint")
	if !drainedReason(sub.C(), NodeChangeScheduling) {
		t.Fatal("the hold was not announced as a scheduling change")
	}
	wantScheduling(t, n.report(dockerOK), NodeCordoned, "maint")
	if got := s.HoldOf(ptr(n.row())); got.Code != "MAINT_WINDOW" {
		t.Fatalf("HoldOf = %+v", got)
	}
}

// The platform's lift does not clear a hold, and an operator cannot turn a
// held node back on (ErrNodeHeld carries the holder's message). They may
// still cordon or drain it, and once the hold ends their uncordon works.
func TestAHoldSurvivesThePlatformLiftAndAnOperatorUncordon(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("maint", "MAINT_WINDOW")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	n := newHoldNode(t, s, "veto")
	blocked := proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing}
	wantScheduling(t, n.report(blocked), NodeCordoned, SchedulingByPlatform)
	h.set("veto", true)
	wantScheduling(t, n.report(blocked), NodeCordoned, "maint")
	wantScheduling(t, n.report(dockerOK), NodeCordoned, "maint")

	_, err := s.SetNodeScheduling("veto", NodeSchedulable)
	var pe *messages.ProductError
	if !errors.Is(err, ErrNodeHeld) || !errors.As(err, &pe) || pe.Code() != "MAINT_WINDOW" || pe.Params()["node"] != "veto" {
		t.Fatalf("uncordon of a held node: err = %v", err)
	}
	wantScheduling(t, n.row(), NodeCordoned, "maint")

	for _, to := range []NodeScheduling{NodeDraining, NodeCordoned} {
		if _, err := s.SetNodeScheduling("veto", to); err != nil {
			t.Fatalf("operator %s of a held node: %v", to, err)
		}
		wantScheduling(t, n.row(), to, SchedulingByOperator)
	}
	wantScheduling(t, n.report(dockerOK), NodeCordoned, SchedulingByOperator)
	if _, err := s.SetNodeScheduling("veto", NodeSchedulable); !errors.Is(err, ErrNodeHeld) {
		t.Fatalf("uncordon of an operator-cordoned held node: err = %v", err)
	}

	h.set("veto", false)
	if _, err := s.SetNodeScheduling("veto", NodeSchedulable); err != nil {
		t.Fatalf("uncordon once the hold ended: %v", err)
	}
	wantScheduling(t, n.report(dockerOK), NodeSchedulable, SchedulingBySelf)
}

// A hold whose holder names no code is shown as NODE_SCHEDULING_HELD.
func TestAnUncodedHoldIsShownAsNodeSchedulingHeld(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("freeze", "")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	n := newHoldNode(t, s, "uncoded")
	h.set("uncoded", true)
	n.report(dockerOK)
	_, err := s.SetNodeScheduling("uncoded", NodeSchedulable)
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorNodeSchedulingHeld {
		t.Fatalf("err = %v", err)
	}
}

// An ended hold falls back to the platform's cordon while any of its three
// reasons remains, and to schedulable when none does.
func TestAnEndedHoldFallsBackToThePlatformsReason(t *testing.T) {
	reboot := []proto.GPUInfo{{Index: 0, UUID: "GPU-r", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: false, HealthCode: accelerator.HealthRebootRequired}}
	for _, tc := range []struct {
		name  string
		sys   proto.SysInfo
		cards []proto.GPUInfo
		want  string
	}{
		{"kube_unschedulable", proto.SysInfo{RuntimeDriver: "docker", KubeUnschedulable: true}, nil, CordonKubeUnschedulable},
		{"runtime_gpu_blocked", proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing}, nil, CordonRuntimeGPUBlocked},
		{"card_needs_host_reboot", dockerOK, reboot, CordonCardNeedsHostReboot},
		{"none", dockerOK, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			h := newSetHolder("maint", "MAINT_WINDOW")
			if err := s.AddSchedulingHolder(h); err != nil {
				t.Fatal(err)
			}
			n := newHoldNode(t, s, "fallback")
			h.set("fallback", true)
			wantScheduling(t, n.report(tc.sys, tc.cards...), NodeCordoned, "maint")

			h.set("fallback", false)
			after := n.report(tc.sys, tc.cards...)
			withCards, _ := s.NodeWithGPUs("fallback")
			if tc.want == "" {
				wantScheduling(t, after, NodeSchedulable, SchedulingBySelf)
				return
			}
			wantScheduling(t, after, NodeCordoned, SchedulingByPlatform)
			if got := withCards.CordonReason(); got != tc.want {
				t.Fatalf("cordon reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// With two holders, the first registered that holds a node owns it; when
// it lets go, the next one takes over without the node becoming schedulable.
func TestTheFirstRegisteredHolderOwnsTheNode(t *testing.T) {
	s := newTestStore(t)
	first, second := newSetHolder("maint", "MAINT_WINDOW"), newSetHolder("freeze", "CHANGE_FREEZE")
	for _, h := range []SchedulingHolder{first, second} {
		if err := s.AddSchedulingHolder(h); err != nil {
			t.Fatal(err)
		}
	}
	n := newHoldNode(t, s, "both")
	first.set("both", true)
	second.set("both", true)
	wantScheduling(t, n.report(dockerOK), NodeCordoned, "maint")
	first.set("both", false)
	wantScheduling(t, n.report(dockerOK), NodeCordoned, "freeze")
	if _, err := s.SetNodeScheduling("both", NodeSchedulable); !errors.Is(err, ErrNodeHeld) {
		t.Fatalf("err = %v", err)
	}
}

// ApplySchedulingHolds applies a holder's answers to every node at once
// and announces each change; operator cordons and drains are left alone.
func TestApplySchedulingHoldsHoldsAndReleases(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("maint", "MAINT_WINDOW")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	a, b, op, drain := newHoldNode(t, s, "a"), newHoldNode(t, s, "b"), newHoldNode(t, s, "op"), newHoldNode(t, s, "drain")
	for _, n := range []holdNode{a, b, op, drain} {
		n.report(dockerOK)
		h.set(n.uuid, true)
	}
	h.set("b", false)
	if _, err := s.SetNodeScheduling("op", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetNodeScheduling("drain", NodeDraining); err != nil {
		t.Fatal(err)
	}
	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	held, released, err := s.ApplySchedulingHolds()
	if err != nil || len(held) != 1 || held[0] != "a" || len(released) != 0 {
		t.Fatalf("held=%v released=%v err=%v", held, released, err)
	}
	if ev := <-sub.C(); ev.Key != "a" || ev.Reason != NodeChangeScheduling {
		t.Fatalf("event = %+v", ev)
	}
	wantScheduling(t, a.row(), NodeCordoned, "maint")
	wantScheduling(t, b.row(), NodeSchedulable, SchedulingBySelf)
	wantScheduling(t, op.row(), NodeCordoned, SchedulingByOperator)
	wantScheduling(t, drain.row(), NodeDraining, SchedulingByOperator)

	h.set("a", false)
	held, released, err = s.ApplySchedulingHolds()
	if err != nil || len(held) != 0 || len(released) != 1 || released[0] != "a" {
		t.Fatalf("held=%v released=%v err=%v", held, released, err)
	}
	wantScheduling(t, a.row(), NodeSchedulable, SchedulingBySelf)
	if held, released, err = s.ApplySchedulingHolds(); err != nil || len(held)+len(released) != 0 {
		t.Fatalf("a second pass changed held=%v released=%v err=%v", held, released, err)
	}
}

// ApplySchedulingHoldsFor touches only the nodes it is given: a node whose
// answer changed but is not named keeps its cordon state until a pass that
// names it, and an id that no longer exists is skipped.
func TestApplySchedulingHoldsForTouchesOnlyTheNamedNodes(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("maint", "MAINT_WINDOW")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	a, b := newHoldNode(t, s, "a"), newHoldNode(t, s, "b")
	ra, rb := a.report(dockerOK), b.report(dockerOK)
	h.set("a", true)
	h.set("b", true)
	if held, released, err := s.ApplySchedulingHoldsFor(nil); err != nil || len(held)+len(released) != 0 {
		t.Fatalf("no ids: held=%v released=%v err=%v", held, released, err)
	}
	held, released, err := s.ApplySchedulingHoldsFor([]uint{ra.ID, 1 << 30})
	if err != nil || len(held) != 1 || held[0] != "a" || len(released) != 0 {
		t.Fatalf("held=%v released=%v err=%v", held, released, err)
	}
	wantScheduling(t, a.row(), NodeCordoned, "maint")
	wantScheduling(t, b.row(), NodeSchedulable, SchedulingBySelf)

	h.set("a", false)
	held, released, err = s.ApplySchedulingHoldsFor([]uint{ra.ID, rb.ID})
	if err != nil || len(held) != 1 || held[0] != "b" || len(released) != 1 || released[0] != "a" {
		t.Fatalf("held=%v released=%v err=%v", held, released, err)
	}
	wantScheduling(t, a.row(), NodeSchedulable, SchedulingBySelf)
	wantScheduling(t, b.row(), NodeCordoned, "maint")
}

// A server without the holder that cordoned a node releases it at boot:
// to the platform's cordon if it has a reason, else schedulable. Sources
// named as known, operator cordons and the platform's are left alone.
func TestReleaseUnownedHoldsReleasesOrphans(t *testing.T) {
	s := newTestStore(t)
	orphan, kept, platform, op, reboot := newHoldNode(t, s, "orphan"), newHoldNode(t, s, "kept"), newHoldNode(t, s, "platform"), newHoldNode(t, s, "op"), newHoldNode(t, s, "reboot")
	for _, n := range []holdNode{orphan, kept, op} {
		n.report(dockerOK)
	}
	platform.report(proto.SysInfo{RuntimeDriver: "docker", KubeUnschedulable: true})
	reboot.report(dockerOK, proto.GPUInfo{Index: 0, UUID: "GPU-r", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", HealthCode: accelerator.HealthRebootRequired})
	setBy := func(uuid string, by SchedulingSource) {
		if err := s.db.Model(&Node{}).Where("uuid = ?", uuid).Updates(map[string]any{"scheduling": NodeCordoned, "scheduling_by": by}).Error; err != nil {
			t.Fatal(err)
		}
	}
	setBy("orphan", "gone")
	setBy("kept", "maint")
	setBy("reboot", "gone")
	if _, err := s.SetNodeScheduling("op", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	released, err := s.ReleaseUnownedHolds("maint")
	if err != nil || len(released) != 2 || released[0] != "orphan" || released[1] != "reboot" {
		t.Fatalf("released=%v err=%v", released, err)
	}
	wantScheduling(t, orphan.row(), NodeSchedulable, SchedulingBySelf)
	wantScheduling(t, reboot.row(), NodeCordoned, SchedulingByPlatform)
	wantScheduling(t, kept.row(), NodeCordoned, "maint")
	wantScheduling(t, platform.row(), NodeCordoned, SchedulingByPlatform)
	wantScheduling(t, op.row(), NodeCordoned, SchedulingByOperator)

	// Without a holder, a report releases an orphan too.
	if released, err := s.ReleaseUnownedHolds(); err != nil || len(released) != 1 {
		t.Fatalf("released=%v err=%v", released, err)
	}
	setBy("orphan", "gone")
	wantScheduling(t, orphan.report(dockerOK), NodeSchedulable, SchedulingBySelf)
}

// Kubernetes "leave" cordons as an operator, which a hold never touches,
// and holds and the platform's reasons combine as decideScheduling says.
func TestAKubeLeaveCordonOutranksAHold(t *testing.T) {
	s := newTestStore(t)
	h := newSetHolder("maint", "MAINT_WINDOW")
	if err := s.AddSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	n := newHoldNode(t, s, "leave")
	h.set("leave", true)
	wantScheduling(t, n.report(proto.SysInfo{RuntimeDriver: "docker", KubeUnschedulable: true}), NodeCordoned, "maint")
	if err := s.db.Transaction(func(tx *gorm.DB) error { return cordonNodesTx(tx, []string{"leave"}) }); err != nil {
		t.Fatal(err)
	}
	wantScheduling(t, n.report(proto.SysInfo{RuntimeDriver: "docker", KubeUnschedulable: true}), NodeCordoned, SchedulingByOperator)
	h.set("leave", false)
	wantScheduling(t, n.report(dockerOK), NodeCordoned, SchedulingByOperator)
	if held, released, err := s.ApplySchedulingHolds(); err != nil || len(held)+len(released) != 0 {
		t.Fatalf("held=%v released=%v err=%v", held, released, err)
	}
}

// Asking the holders about a node they do not hold allocates nothing: it
// runs on every status report of every node.
func TestHoldOfAllocatesNothingWhenNotHeld(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddSchedulingHolder(newSetHolder("maint", "MAINT_WINDOW")); err != nil {
		t.Fatal(err)
	}
	facts := NodeHoldFacts{ID: 1, UUID: "free"}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = s.admission.holdOf(facts) }); allocs != 0 {
		t.Fatalf("holdOf allocated %v times per call", allocs)
	}
}

func drainedReason(c <-chan bus.Event, reason string) bool {
	for {
		select {
		case ev := <-c:
			if ev.Reason == reason {
				return true
			}
		default:
			return false
		}
	}
}

func ptr[T any](v T) *T { return &v }
