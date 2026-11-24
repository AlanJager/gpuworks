package store

import (
	"strings"
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

// A peer sync's progress comes from the receiving node, about once a second,
// and its report of the loaded image carries no count; the sender's result
// arrives last. The finished row must hold the sender's count, whichever of
// the two the server handles first, and an archive size nobody knew.
func TestRecordImageTransferBytesFinishesAPeerSyncWithTheSendersCount(t *testing.T) {
	const archive = int64(8388608)
	for _, c := range []struct {
		name          string
		size          int64
		recordFirst   bool
		wantSizeBytes int64
	}{
		{"load report first", archive, false, archive},
		{"sender's result first", archive, true, archive},
		{"archive size unknown", 0, false, archive},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, err := Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			image, _ := st.UpsertImage(&Image{ImageRef: "example/image:count", Engine: "vllm", Enabled: true})
			node, _ := st.RegisterNode("node-count", HashToken("t"), "count")
			if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "relay", Source: "peer"}); err != nil {
				t.Fatal(err)
			}
			// What the receiving node sends: a sample, then the loaded image.
			sample := proto.ImageNodeStatus{State: ImageStateSyncing, TransferID: "relay", Source: "peer", SizeBytes: c.size, TransferredBytes: 5545984, RateBPS: 1000}
			loaded := proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "relay", Message: "image archive loaded"}
			if err := st.ApplyImageNodeStatus(image.ID, node.ID, sample); err != nil {
				t.Fatal(err)
			}
			record := func() {
				if ok, err := st.RecordImageTransferBytes(image.ID, node.ID, "relay", archive); err != nil || !ok {
					t.Fatalf("record = %v, %v", ok, err)
				}
			}
			if c.recordFirst {
				record()
				// A sample the node sent earlier, handled after the final count.
				if err := st.ApplyImageNodeStatus(image.ID, node.ID, sample); err != nil {
					t.Fatal(err)
				}
				if row, _ := st.ImageNode(image.ID, node.ID); row.TransferredBytes != archive {
					t.Fatalf("a late sample lowered the count to %d", row.TransferredBytes)
				}
			}
			if err := st.ApplyImageNodeStatus(image.ID, node.ID, loaded); err != nil {
				t.Fatal(err)
			}
			if !c.recordFirst {
				if row, _ := st.ImageNode(image.ID, node.ID); row.TransferredBytes != 5545984 {
					t.Fatalf("before the sender's result the row holds %d, want the sample", row.TransferredBytes)
				}
				record()
			}
			// Nothing the node can still send for this transfer changes it.
			for _, late := range []proto.ImageNodeStatus{sample, loaded} {
				if err := st.ApplyImageNodeStatus(image.ID, node.ID, late); err != nil {
					t.Fatal(err)
				}
			}
			row, _ := st.ImageNode(image.ID, node.ID)
			if row.State != ImageStatePresent || row.TransferredBytes != archive || row.SizeBytes != c.wantSizeBytes {
				t.Fatalf("finished row = %+v, want present with %d of %d", row, archive, c.wantSizeBytes)
			}
		})
	}
}

// The count belongs to one transfer: it is not written on a row another
// transfer owns or finished, nor on a failed one.
func TestRecordImageTransferBytesLeavesOtherTransfersAlone(t *testing.T) {
	st, err := Open("file:store_image_count_other?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, _ := st.UpsertImage(&Image{ImageRef: "example/image:other", Engine: "vllm", Enabled: true})
	node, _ := st.RegisterNode("node-other", HashToken("t"), "other")
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "second", Source: "peer", SizeBytes: 100, TransferredBytes: 10}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", ""} {
		if ok, err := st.RecordImageTransferBytes(image.ID, node.ID, id, 100); err != nil || ok {
			t.Fatalf("record for %q on another transfer's row = %v, %v", id, ok, err)
		}
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStateFailed, TransferID: "second", Message: "load failed"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RecordImageTransferBytes(image.ID, node.ID, "second", 100); err != nil || ok {
		t.Fatalf("record on a failed row = %v, %v", ok, err)
	}
	if row, _ := st.ImageNode(image.ID, node.ID); row.TransferredBytes != 10 {
		t.Fatalf("row = %+v, want the count untouched", row)
	}
}
