package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A relay through the server records its progress only while the row is
// syncing. The target reports its load on its own stream, so that report can
// finish the row before the relay has written its last count, which is then
// dropped. The finished row must still hold everything the relay carried.
func TestRelayedImageKeepsItsWholeCountWhenTheLoadReportComesFirst(t *testing.T) {
	previous := imageTransferConfirmWait
	imageTransferConfirmWait = 50 * time.Millisecond
	defer func() { imageTransferConfirmWait = previous }()
	h := newRelayHub(t, "file:image_relay_count?mode=memory&cache=shared", "/image.tar")
	const size, sampled = int64(8 << 20), int64(5 << 20)
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStateSyncing, Source: "url-relay", TransferID: "relay"}); err != nil {
		t.Fatal(err)
	}
	h.hub.trackImageTransfer(context.Background(), h.image.ID, h.node.ID, "relay", size, func(_ context.Context, progress func(int64)) (int64, error) {
		progress(sampled)
		// The target's report of the loaded image, handled before the copy
		// returns its count.
		if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{State: store.ImageStatePresent, TransferID: "relay", Message: "image archive loaded"}); err != nil {
			t.Error(err)
		}
		return size, nil
	})
	waitFor(t, 5*time.Second, func() bool {
		row, err := h.store.ImageNode(h.image.ID, h.node.ID)
		return err == nil && row.State == store.ImageStatePresent && row.TransferredBytes == size
	}, "the relayed image's whole count")
	row, _ := h.store.ImageNode(h.image.ID, h.node.ID)
	if row.SizeBytes != size || row.Source != "url-relay" {
		t.Fatalf("finished row = %+v", row)
	}
}

// A peer copy's count comes from the receiving node as samples; the sending
// side's result carries the whole archive. When the copy returns, the row
// takes that count: on the row still syncing, and on the row the node's load
// report has already finished.
func TestFinishedPeerCopyRecordsTheSendersCount(t *testing.T) {
	const size, sampled = int64(8 << 20), int64(5 << 20)
	for _, c := range []struct {
		name         string
		loadedFirst  bool
		result       proto.ImageSyncCopyResult
		wantState    string
		wantCount    int64
		wantLastID   string
		wantActiveID string
	}{
		{"copy returns while the row is syncing", false, proto.ImageSyncCopyResult{TransferID: "copy", Bytes: size}, store.ImageStateSyncing, size, "", "copy"},
		{"load report handled first", true, proto.ImageSyncCopyResult{TransferID: "copy", Bytes: size}, store.ImageStatePresent, size, "copy", ""},
		{"target already receiving this transfer", false, proto.ImageSyncCopyResult{TransferID: "copy", InProgress: true}, store.ImageStateSyncing, sampled, "", "copy"},
		{"archive over the limit", false, proto.ImageSyncCopyResult{TransferID: "copy", Bytes: imageArchiveMaxBytes + 1}, store.ImageStateFailed, sampled, "copy", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, image, source, target := imageStallFixture(t, "peer_count_"+strings.NewReplacer(" ", "_").Replace(c.name))
			if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, SourceNodeID: source.ID, Source: "peer", State: store.ImageStateSyncing, TransferID: "copy"}); err != nil {
				t.Fatal(err)
			}
			// The receiving node's sample, as it sends it.
			if err := h.store.ApplyImageNodeStatus(image.ID, target.ID, proto.ImageNodeStatus{State: store.ImageStateSyncing, TransferID: "copy", Source: "peer", SizeBytes: size, TransferredBytes: sampled}); err != nil {
				t.Fatal(err)
			}
			if c.loadedFirst {
				if err := h.store.ApplyImageNodeStatus(image.ID, target.ID, proto.ImageNodeStatus{State: store.ImageStatePresent, TransferID: "copy", Message: "image archive loaded"}); err != nil {
					t.Fatal(err)
				}
			}
			// The confirmation wait is cancelled: only what the copy's return
			// records is under test.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h.finishDirectImageSync(ctx, image.ID, target.ID, "copy", c.result, time.Minute)
			row, err := h.store.ImageNode(image.ID, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != c.wantState || row.TransferredBytes != c.wantCount || row.TransferID != c.wantActiveID || row.LastTransferID != c.wantLastID {
				t.Fatalf("row = state %s, %d transferred, transfer %q, last %q; want %s, %d, %q, %q", row.State, row.TransferredBytes, row.TransferID, row.LastTransferID, c.wantState, c.wantCount, c.wantActiveID, c.wantLastID)
			}
		})
	}
}
