package planning_test

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// Equal GPU indexes on different nodes are different budgets. The selected
// replica on the second node must account for its own co-tenant, even when
// the first node has enough room for the new quota.
func TestQuotaAcrossNodesBySelectedReplicaAndHolderState(t *testing.T) {
	for _, state := range []struct {
		name, state, phase string
		holds              bool
	}{
		{"pending", proto.InstPending, "", true}, {"removing", proto.InstRemoving, "", true},
		{"failed-sleep", proto.InstFailed, proto.PhaseSleep, true}, {"failed-launch", proto.InstFailed, "pull", false}, {"stopped", proto.InstStopped, "", false},
	} {
		for _, both := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/both-%v", state.name, both), func(t *testing.T) {
				w := fixtures.NewWorld(t)
				remote := w
				remote.Node = w.AddNode(t, "second-node", 2)
				dep := w.Deployment(t, "slices", 16*gib)
				w.Instance(t, dep, "first", proto.InstRunning, "", 0, 0)
				remote.Instance(t, dep, "second", proto.InstRunning, "", 0, 0)
				other := remote.Deployment(t, "other", 16*gib)
				remote.Instance(t, other, "co-tenant", state.state, state.phase, 0, 0)
				live := []string{"first"}
				if both {
					live = append(live, "second")
				}
				for _, limit := range []int64{8 * gib, 48 * gib} {
					short, err := planning.QuotaRoom(w.Store, dep, limit, live)
					wantShort := both && state.holds && limit == 48*gib
					if err != nil || (short != nil) != wantShort {
						t.Fatalf("limit=%d shortfall=%+v err=%v, want shortfall=%v", limit, short, err, wantShort)
					}
					if wantShort && (short.Node.ID != remote.Node.ID || short.GPUIndex != 0 || short.FreeBytes != 64*gib-fixtures.NVIDIASliceClaim(16*gib) || short.NeedBytes != fixtures.NVIDIASliceClaim(48*gib)) {
						t.Fatalf("wrong node's budget: %+v", short)
					}
				}
				for _, world := range []fixtures.World{w, remote} {
					want := fixtures.NVIDIASliceClaim(16 * gib)
					if world.Node.ID == remote.Node.ID && state.holds {
						want = 2 * fixtures.NVIDIASliceClaim(16*gib)
					}
					if got := planning.GPUAllocations(w.Store, world.Node)[0].AllocatedBytes; got != want {
						t.Fatalf("quota preview changed node %d claim: %d, want %d", world.Node.ID, got, want)
					}
				}
			})
		}
	}
}
