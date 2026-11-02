package store

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// The watchdog may only ever fail a transfer that is still in progress: a row
// that already finished, however old, must keep its result.
func TestFailImageNodeIfUnchangedLeavesFinishedRowsAlone(t *testing.T) {
	st, err := Open("file:image_node_stale_guard?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&Image{ImageRef: "quay.io/team/runner:stale", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-stale")
	if err != nil {
		t.Fatal(err)
	}
	present, err := st.UpsertImageNode(&ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: ImageStatePresent, Message: "loaded", SizeBytes: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&ImageNode{}).Where("id = ?", present.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	changed, err := st.FailImageNodeIfUnchanged(image.ID, node.ID, "", time.Now(), "abandoned")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a finished row was failed by the watchdog")
	}
	after, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != ImageStatePresent || after.SizeBytes != 42 || after.Message != "loaded" {
		t.Fatalf("finished row = %+v", after)
	}
}
