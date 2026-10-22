package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestImageSyncRetryClearsPreviousProgress(t *testing.T) {
	st, err := Open("file:store_image_sync_retry?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:1", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-image", HashToken("t"), "image")
	row, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateFailed, SizeBytes: 100, TransferredBytes: 75, RateBPS: 10, TransferID: "old", Message: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&ImageNode{ID: row.ID, ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, SizeBytes: 100, TransferID: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TransferredBytes != 0 || got.RateBPS != 0 || got.TransferID != "new" || got.Message != "" {
		t.Fatalf("retry row = %+v", got)
	}
}

func TestImagePresenceDoesNotEraseActiveSync(t *testing.T) {
	st, err := Open("file:store_image_presence_sync?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:presence", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-presence", HashToken("t"), "presence")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "active", TransferredBytes: 12}); err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{{ImageRef: image.ImageRef, Present: false}}}); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateSyncing || row.TransferID != "active" || row.TransferredBytes != 12 {
		t.Fatalf("presence changed active transfer: %+v", row)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{{ImageRef: image.ImageRef, Present: true, SizeBytes: 100}}}); err != nil {
		t.Fatal(err)
	}
	row, err = st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateSyncing || row.TransferID != "active" || row.TransferredBytes != 12 {
		t.Fatalf("present inventory completed active transfer: %+v", row)
	}
}

func TestImagePresenceDoesNotEraseFailedSync(t *testing.T) {
	st, err := Open("file:store_image_presence_failed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:failed", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-failed", HashToken("t"), "failed")
	if _, err := st.UpsertImageNode(&ImageNode{
		ImageID:          image.ID,
		NodeID:           node.ID,
		State:            ImageStateFailed,
		TransferredBytes: 42,
		Message:          "docker load completed but image is not present",
	}); err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUProbeOK: &ok, Images: []proto.ImagePresence{{ImageRef: image.ImageRef, Present: false}}}); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStateFailed || row.TransferredBytes != 42 || row.Message != "docker load completed but image is not present" {
		t.Fatalf("presence changed failed transfer: %+v", row)
	}
}

func TestFailImageSyncIfActiveIgnoresStaleTransfer(t *testing.T) {
	st, err := Open("file:store_image_sync_stale?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:2", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-image", HashToken("t"), "image")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "current"}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailImageSyncIfActive(image.ID, node.ID, "stale", "timeout"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ImageNode(image.ID, node.ID)
	if got.State != ImageStateSyncing || got.TransferID != "current" {
		t.Fatalf("stale timeout changed row: %+v", got)
	}
}

func TestImageSyncProgressCannotOverwriteTerminalOrNewerTransfer(t *testing.T) {
	st, err := Open("file:store_image_sync_progress_cas?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:progress", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-progress", HashToken("t"), "progress")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "current"}); err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpdateImageSyncProgressIfActive(image.ID, node.ID, "current", 100, 50, 10)
	if err != nil || !updated {
		t.Fatalf("active progress updated=%v err=%v", updated, err)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "current", SizeBytes: 100, TransferredBytes: 100}); err != nil {
		t.Fatal(err)
	}
	updated, err = st.UpdateImageSyncProgressIfActive(image.ID, node.ID, "current", 100, 75, 5)
	if err != nil || updated {
		t.Fatalf("terminal state accepted late progress: updated=%v err=%v", updated, err)
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != ImageStatePresent || row.TransferredBytes != 100 || row.TransferID != "" {
		t.Fatalf("late progress changed terminal row: %+v", row)
	}
}

// The agent's receiver flushes its final byte count after the load reported
// the terminal state. That frame still names the finished transfer and must not
// reopen the row, while a later terminal report is still accepted.
func TestApplyImageNodeStatusIgnoresLateProgressForFinishedTransfer(t *testing.T) {
	st, err := Open("file:store_image_late_status?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:late", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-late", HashToken("t"), "late")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "done", Source: "peer", RateLimitKiB: 1024}); err != nil {
		t.Fatal(err)
	}
	for _, st2 := range []proto.ImageNodeStatus{
		{State: ImageStatePresent, TransferID: "done"},
		{State: ImageStateSyncing, TransferID: "done", TransferredBytes: 100, Source: "peer"},
	} {
		if err := st.ApplyImageNodeStatus(image.ID, node.ID, st2); err != nil {
			t.Fatal(err)
		}
	}
	row, _ := st.ImageNode(image.ID, node.ID)
	if row.State != ImageStatePresent || row.TransferID != "" || row.LastRateLimitKiB != 1024 {
		t.Fatalf("late progress reopened finished row: %+v", row)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStateFailed, TransferID: "done", Message: "load failed"}); err != nil {
		t.Fatal(err)
	}
	if row, _ = st.ImageNode(image.ID, node.ID); row.State != ImageStateFailed {
		t.Fatalf("terminal report after finish was dropped: %+v", row)
	}
}
