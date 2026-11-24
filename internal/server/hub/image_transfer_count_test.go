package hub

import (
	"context"
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
