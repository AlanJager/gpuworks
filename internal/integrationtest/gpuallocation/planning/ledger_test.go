package planning_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

const gib = fixtures.GiB

type allocationExpectation struct {
	allocated, sleeping int64
	whole               bool
	instances           int
}

func assertAllocation(t *testing.T, w fixtures.World, want allocationExpectation) {
	t.Helper()
	got := planning.GPUAllocations(w.Store, w.Node)
	a := got[0]
	if a.AllocatedBytes != want.allocated || a.SleepingBytes != want.sleeping || a.WholeGPUAllocated != want.whole || len(a.Instances) != want.instances {
		t.Fatalf("ledger=%+v, want %+v", a, want)
	}
	if other := got[1]; other.AllocatedBytes != 0 || other.SleepingBytes != 0 || len(other.Instances) != 0 {
		t.Fatalf("claim leaked to another card: %+v", other)
	}
	for _, card := range planning.NodeSnapshot(w.Store, w.Node, true, "").GPUs {
		if card.Index == 0 && (card.AllocatedBytes != want.allocated || card.SleepingBytes != want.sleeping || card.WholeGPUAllocated != want.whole) {
			t.Fatalf("snapshot lost ledger facts: %+v", card)
		}
	}
}

// Each claim kind with the bytes it holds on a 64 GiB card while awake.
var claimKinds = []struct {
	name                string
	limit, claim, bytes int64
	whole               bool
}{
	{"whole", 0, 0, 64 * gib, true}, {"partition", 16 * gib, 0, fixtures.NVIDIASliceClaim(16 * gib), false}, {"native", 0, 20 * gib, 20 * gib, false},
}

// Unlike candidate availability, this matrix checks the exact accounting and
// retention of each claim kind across every claim-bearing/terminal state.
func TestClaimKindByInstanceState(t *testing.T) {
	for _, mode := range claimKinds {
		for _, state := range []struct {
			name, state, phase string
			holds              bool
		}{
			{"pending", proto.InstPending, "", true}, {"pulling", proto.InstPulling, "", true}, {"starting", proto.InstStarting, "", true},
			{"optimizing", proto.InstOptimizing, "", true}, {"running", proto.InstRunning, "", true},
			{"draining", proto.InstDraining, "", true}, {"pending-removal", proto.InstPendingRemoval, "", true}, {"removing", proto.InstRemoving, "", true},
			{"failed-sleep", proto.InstFailed, proto.PhaseSleep, true}, {"failed-launch", proto.InstFailed, "pull", false}, {"stopped", proto.InstStopped, "", false},
		} {
			t.Run(mode.name+"/"+state.name, func(t *testing.T) {
				w := fixtures.NewWorld(t)
				dep := w.Deployment(t, "holder", mode.limit)
				w.Instance(t, dep, "holder", state.state, state.phase, 0, mode.claim)
				want := allocationExpectation{}
				if state.holds {
					want = allocationExpectation{allocated: mode.bytes, whole: mode.whole, instances: 1}
				}
				assertAllocation(t, w, want)
			})
		}
	}
}

// Sleep/wake is a sequence: an order does not free memory before the report,
// and a wake reserves its full claim before the engine has reported awake.
func TestClaimKindThroughSleepAndWake(t *testing.T) {
	for _, mode := range claimKinds {
		t.Run(mode.name, func(t *testing.T) {
			w := fixtures.NewWorld(t)
			dep := w.Deployment(t, "holder", mode.limit)
			w.Instance(t, dep, "holder", proto.InstRunning, "", 0, mode.claim)
			full := allocationExpectation{allocated: mode.bytes, whole: mode.whole, instances: 1}
			assertAllocation(t, w, full)
			if err := w.Store.SetDeploymentSleep(dep.ID, 2); err != nil {
				t.Fatal(err)
			}
			assertAllocation(t, w, full)
			if err := w.Store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "holder", State: proto.InstRunning, SleepLevel: 2, SleepResidueBytes: gib}); err != nil {
				t.Fatal(err)
			}
			assertAllocation(t, w, allocationExpectation{sleeping: gib, instances: 1})
			if err := w.Store.SetDeploymentSleep(dep.ID, 0); err != nil {
				t.Fatal(err)
			}
			assertAllocation(t, w, full)
			if err := w.Store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "holder", State: proto.InstRunning}); err != nil {
				t.Fatal(err)
			}
			assertAllocation(t, w, full)
			if err := w.Store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "holder", State: proto.InstStopped}); err != nil {
				t.Fatal(err)
			}
			assertAllocation(t, w, allocationExpectation{})
		})
	}
}
