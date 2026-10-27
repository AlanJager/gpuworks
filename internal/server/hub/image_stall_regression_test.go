package hub

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func imageStallFixture(t *testing.T, name string) (*Hub, *store.Image, *store.Node, *store.Node) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "fixture/image:" + name, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.RegisterNode(name+"source", store.HashToken("source"), "source")
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.RegisterNode(name+"target", store.HashToken("target"), "target")
	if err != nil {
		t.Fatal(err)
	}
	return New(st, slog.New(slog.NewTextHandler(io.Discard, nil))), image, source, target
}

func TestAbandonedImageSnapshotCannotCancelNewPeerAttempt(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "stale_snapshot")
	old, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "new"}); err != nil {
		t.Fatal(err)
	}
	h.abandonDirectImageTransfer(image.ID, target.ID, old)
	current, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil || current.State != store.ImageStateSyncing || current.TransferID != "new" {
		t.Fatalf("old watchdog cancelled newer attempt: %+v err=%v", current, err)
	}
}

func TestResumingPeerImageSyncRestoresStallWatch(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "resume_watch")
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "restored"}); err != nil {
		t.Fatal(err)
	}
	previousCheck, previousAfter := imageDirectStallCheck, imageDirectStallAfter
	imageDirectStallCheck, imageDirectStallAfter = 5*time.Millisecond, 40*time.Millisecond
	defer func() { imageDirectStallCheck, imageDirectStallAfter = previousCheck, previousAfter }()
	h.sessions[target.UUID] = &Session{}
	if _, err := h.SyncImageFromRate(context.Background(), image.ID, target.UUID, source.UUID, 0); err != nil {
		t.Fatal(err)
	}
	// Reconnect then goes away: the persisted attempt must still become retryable.
	h.mu.Lock()
	delete(h.sessions, target.UUID)
	h.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		row, err := h.store.ImageNode(image.ID, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == store.ImageStateFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("resumed peer transfer remained syncing after its progress stopped")
}

func TestOldImageWatchDoesNotCancelAdvancedProgressOrReusedTimestamp(t *testing.T) {
	for _, newer := range []bool{false, true} {
		name := "advanced_progress"
		if newer {
			name = "new_id_same_time"
		}
		t.Run(name, func(t *testing.T) {
			h, image, source, target := imageStallFixture(t, name)
			old, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "old"})
			if err != nil {
				t.Fatal(err)
			}
			id := "old"
			if newer {
				id = "new"
				if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: id}); err != nil {
					t.Fatal(err)
				}
				if err := h.store.DB().Model(&store.ImageNode{}).Where("id = ?", old.ID).Update("updated_at", old.UpdatedAt).Error; err != nil {
					t.Fatal(err)
				}
			} else if _, err := h.store.UpdateImageSyncProgressIfActive(image.ID, target.ID, id, 100, 50, 1); err != nil {
				t.Fatal(err)
			}
			h.abandonDirectImageTransfer(image.ID, target.ID, old)
			row, err := h.store.ImageNode(image.ID, target.ID)
			if err != nil || row.State != store.ImageStateSyncing || row.TransferID != id {
				t.Fatalf("stale observation cancelled live attempt: %+v %v", row, err)
			}
		})
	}
}

func TestImageWatchSharesOneAttemptAndReleasesAfterCompletion(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "watch_dedup")
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "one"}); err != nil {
		t.Fatal(err)
	}
	previousCheck := imageDirectStallCheck
	imageDirectStallCheck = 5 * time.Millisecond
	defer func() { imageDirectStallCheck = previousCheck }()
	h.WatchImageTransfer(image.ID, target.ID)
	h.WatchImageTransfer(image.ID, target.ID)
	h.mu.RLock()
	watches := len(h.imageWatches)
	h.mu.RUnlock()
	if watches != 1 {
		t.Fatalf("duplicate watch count=%d", watches)
	}
	if err := h.store.ApplyImageNodeStatus(image.ID, target.ID, proto.ImageNodeStatus{State: store.ImageStatePresent, TransferID: "one"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		remaining := len(h.imageWatches)
		h.mu.RUnlock()
		if remaining == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("completed image watch retained its registration")
}
