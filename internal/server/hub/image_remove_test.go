package hub

import (
	"context"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An offline node cannot delete anything, and the row must keep saying the
// image is there: reporting it gone because the request failed would be a lie
// the operator acts on.
func TestRemoveImageFromNodeRefusesAnOfflineNodeAndKeepsTheRow(t *testing.T) {
	st, err := store.Open("file:hub_remove_offline_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:remove", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-remove")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStatePresent, SizeBytes: 42, Message: "loaded",
	}); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	if _, err := h.RemoveImageFromNode(context.Background(), image.ID, node.UUID); err == nil {
		t.Fatal("removing from an offline node must fail")
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStatePresent || row.SizeBytes != 42 {
		t.Fatalf("row after a refused removal = %+v", row)
	}
}
