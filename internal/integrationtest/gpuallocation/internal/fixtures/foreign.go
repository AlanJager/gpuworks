package fixtures

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// EnableCards records an operator's decision to enable scheduling on every
// card of the node, through the same store call as the card's own action.
func (w World) EnableCards(t *testing.T) {
	t.Helper()
	for _, gpu := range w.Node.GPUs {
		if _, err := w.Store.SetCardScheduling(w.Node.UUID, gpu.Ident, store.CardSchedulingSchedulable); err != nil {
			t.Fatal(err)
		}
	}
	node, err := w.Store.NodeWithGPUs(w.Node.UUID)
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
