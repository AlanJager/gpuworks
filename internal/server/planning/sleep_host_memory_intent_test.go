package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/sleep"
)

// L1HostMemoryRefusal must charge an instance that is still holding its VRAM,
// including one that is only ORDERED to sleep.
//
// This predicate is a resource question, not a routing question, and the two
// must not be conflated: the gateway refusing a replica does not make the
// replica's weights appear in host memory. An instance with a sleep order in
// flight is still at its engine's old level, so its weights have not moved yet
// — they are about to. Reserving host memory for the world after the drain
// while acting on the world before it admits a sleep the host cannot survive,
// and the failure mode is the OOM killer taking the engine down mid-drain
// rather than a clean 409.
func TestL1SleepReservesHostMemoryForASleeperStillAtItsEngine(t *testing.T) {
	st, node, dep := residueFixture(t, 40<<30, "bfloat16")
	// Order the L1 sleep first, then judge it: the second click of a sleep that
	// is still draining is exactly when an instance holds an intent but no
	// reported level yet.
	if err := st.OrderDeploymentSleep(dep.ID, proto.SleepLevelL1, "wait"); err != nil {
		t.Fatal(err)
	}
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := insts[0]
	if in.SleepIntent() != proto.SleepLevelL1 || in.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (%d, 0)", in.SleepIntent(), in.SleepLevel, proto.SleepLevelL1)
	}
	host := func(uuid string) (int64, int64, bool) {
		if uuid != node.UUID {
			t.Fatalf("host memory read for %q", uuid)
		}
		return 1 << 30, 64 << 30, true
	}
	refusal := L1HostMemoryRefusal(st, dep, insts, host)
	if refusal == nil || refusal.Code != sleep.CodeHostMemoryInsufficient {
		t.Fatalf("40 GiB of weights onto 1 GiB free = %+v, want a refusal: the weights are still on the card and are about to move to the host", refusal)
	}
	if refusal.Params["need_gib"] != "40.0" {
		t.Fatalf("need_gib = %v, want the whole 40 GiB: a draining engine has offloaded nothing yet", refusal.Params["need_gib"])
	}

	// Once the engine really is at the level, it has moved them: nothing more
	// to reserve, and the order may proceed.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "residue-1", State: proto.InstRunning, SleepLevel: proto.SleepLevelL1}); err != nil {
		t.Fatal(err)
	}
	insts, err = st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refusal := L1HostMemoryRefusal(st, dep, insts, host); refusal != nil {
		t.Fatalf("an instance already at its level was charged again: %+v", refusal)
	}
}

// An engine that already slept at L1 (the agent's startup prewarm) keeps that
// host memory pinned while awake; the node's available memory already
// excludes it. Charging the weights again refused real L1 sleeps on an H20
// node with three large models (each engine pinned 1.4-1.8x its weights at
// prewarm), and the yield policy's auto level then fell back to L2.
func TestL1SleepDoesNotChargeAHostBufferTheEngineAlreadyHolds(t *testing.T) {
	st, node, dep := residueFixture(t, 40<<30, "bfloat16")
	host := func(uuid string) (int64, int64, bool) {
		if uuid != node.UUID {
			t.Fatalf("host memory read for %q", uuid)
		}
		return 1 << 30, 64 << 30, true
	}
	judge := func(step string, wantRefused bool) {
		t.Helper()
		insts, err := st.InstancesForDeployment(dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		refusal := L1HostMemoryRefusal(st, dep, insts, host)
		if wantRefused && (refusal == nil || refusal.Code != sleep.CodeHostMemoryInsufficient || refusal.Params["need_gib"] != "40.0") {
			t.Fatalf("%s: refusal = %+v, want the whole 40 GiB charged", step, refusal)
		}
		if !wantRefused && refusal != nil {
			t.Fatalf("%s: refused %+v, want nothing charged", step, refusal)
		}
	}
	report := func(st2 proto.InstanceStatus) {
		t.Helper()
		st2.UUID = "residue-1"
		if err := st.UpdateInstanceStatus(st2); err != nil {
			t.Fatal(err)
		}
	}

	judge("not known to hold one", true)

	report(proto.InstanceStatus{State: proto.InstRunning, L1HostBufferHeld: true})
	judge("after the prewarm", false)

	// An agent that restarted adopts the same process and does not know; its
	// report must not clear what the earlier one said.
	report(proto.InstanceStatus{State: proto.InstRunning})
	judge("adopted after an agent restart", false)

	// A sleep result (here a failed one, in the sleep phase) keeps it too.
	report(proto.InstanceStatus{State: proto.InstRunning, Phase: proto.PhaseSleep, L1HostBufferHeld: true})
	judge("after a failed order", false)

	// A new process starts with no buffer: its weights are charged again
	// until its own prewarm says otherwise.
	report(proto.InstanceStatus{State: proto.InstStarting})
	judge("a new process", true)
	report(proto.InstanceStatus{State: proto.InstRunning})
	judge("the new process before its prewarm reports", true)
}
