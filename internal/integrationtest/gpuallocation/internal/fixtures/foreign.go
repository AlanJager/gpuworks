package fixtures

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// AllowForeignSharing uses the same policy update as an operator's edit.
func (w World) AllowForeignSharing(t *testing.T, allow bool) {
	t.Helper()
	_, _, node, err := w.Store.SetNodePlacementPolicy(w.Node.UUID, func(policy store.NodePlacementPolicy) (store.NodePlacementPolicy, error) {
		policy.ShareWithForeign = allow
		return policy, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	*w.Node = *node
}

// HoldForeign reports an outside process on the named cards. The fixture's
// complete prior report keeps the other cards and backend capabilities intact.
func (w World) HoldForeign(t *testing.T, indexes ...int) {
	t.Helper()
	report, ok := w.reports[w.Node.UUID]
	if !ok {
		t.Fatalf("no fixture report for node %s", w.Node.UUID)
	}
	report.GPUs = slices.Clone(report.GPUs)
	for _, index := range indexes {
		position := slices.IndexFunc(report.GPUs, func(g proto.GPUInfo) bool { return g.Index == index })
		if position < 0 {
			t.Fatalf("node %s has no card %d", w.Node.UUID, index)
		}
		report.GPUs[position].ForeignProcesses = []proto.CardProcess{{PID: 42, Name: "outside", MemoryBytes: ForeignHolderBytes}}
	}
	if err := w.Store.UpdateNodeStatus(w.Node.UUID, report); err != nil {
		t.Fatal(err)
	}
	w.reports[w.Node.UUID] = report
	node, err := w.Store.NodeWithGPUs(w.Node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	*w.Node = *node
}
